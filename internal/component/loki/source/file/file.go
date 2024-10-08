package file

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	parser "github.com/coroot/logparser"
	"github.com/grafana/alloy/internal/component"
	"github.com/grafana/alloy/internal/component/common/loki"
	"github.com/grafana/alloy/internal/component/common/loki/positions"
	"github.com/grafana/alloy/internal/component/discovery"
	"github.com/grafana/alloy/internal/featuregate"
	"github.com/grafana/alloy/internal/runtime/logging/level"
	"github.com/grafana/tail/watch"
	"github.com/prometheus/common/model"
	"go.opentelemetry.io/otel/attribute"
	api "go.opentelemetry.io/otel/metric"
)

func init() {
	component.Register(component.Registration{
		Name:      "loki.source.file",
		Stability: featuregate.StabilityGenerallyAvailable,
		Args:      Arguments{},

		Build: func(opts component.Options, args component.Arguments) (component.Component, error) {
			return New(opts, args.(Arguments))
		},
	})
}

const (
	pathLabel     = "__path__"
	filenameLabel = "filename"
)

// Arguments holds values which are used to configure the loki.source.file
// component.
type Arguments struct {
	Targets             []discovery.Target  `alloy:"targets,attr"`
	ForwardTo           []loki.LogsReceiver `alloy:"forward_to,attr"`
	Encoding            string              `alloy:"encoding,attr,optional"`
	DecompressionConfig DecompressionConfig `alloy:"decompression,block,optional"`
	FileWatch           FileWatch           `alloy:"file_watch,block,optional"`
	TailFromEnd         bool                `alloy:"tail_from_end,attr,optional"`
	LegacyPositionsFile string              `alloy:"legacy_positions_file,attr,optional"`
}

type FileWatch struct {
	MinPollFrequency time.Duration `alloy:"min_poll_frequency,attr,optional"`
	MaxPollFrequency time.Duration `alloy:"max_poll_frequency,attr,optional"`
}

var DefaultArguments = Arguments{
	FileWatch: FileWatch{
		MinPollFrequency: 250 * time.Millisecond,
		MaxPollFrequency: 250 * time.Millisecond,
	},
}

// SetToDefault implements syntax.Defaulter.
func (a *Arguments) SetToDefault() {
	*a = DefaultArguments
}

type DecompressionConfig struct {
	Enabled      bool              `alloy:"enabled,attr"`
	InitialDelay time.Duration     `alloy:"initial_delay,attr,optional"`
	Format       CompressionFormat `alloy:"format,attr"`
}

var _ component.Component = (*Component)(nil)

// Component implements the loki.source.file component.
type Component struct {
	opts    component.Options
	metrics *metrics

	updateMut sync.Mutex

	mut       sync.RWMutex
	args      Arguments
	handler   loki.LogsReceiver
	receivers []loki.LogsReceiver
	posFile   positions.Positions
	readers   map[positions.Entry]reader

	lastLogInfo sync.Map
	stopch      chan struct{}
}

var node_name string
var node_ip string

// New creates a new loki.source.file component.
func New(o component.Options, args Arguments) (*Component, error) {
	err := os.MkdirAll(o.DataPath, 0750)
	if err != nil && !os.IsExist(err) {
		return nil, err
	}
	newPositionsPath := filepath.Join(o.DataPath, "positions.yml")
	// Check to see if we can convert the legacy positions file to the new format.
	if args.LegacyPositionsFile != "" {
		positions.ConvertLegacyPositionsFile(args.LegacyPositionsFile, newPositionsPath, o.Logger)
	}
	positionsFile, err := positions.New(o.Logger, positions.Config{
		SyncPeriod:        10 * time.Second,
		PositionsFile:     newPositionsPath,
		IgnoreInvalidYaml: false,
		ReadOnly:          false,
	})
	if err != nil {
		return nil, err
	}

	c := &Component{
		opts:    o,
		metrics: newMetrics(o.Registerer),

		handler:   loki.NewLogsReceiver(),
		receivers: args.ForwardTo,
		posFile:   positionsFile,
		readers:   make(map[positions.Entry]reader),
		stopch:    make(chan struct{}),
	}

	hostname, find := os.LookupEnv("COMPUTERNAME")
	if !find {
		hostname, _ = os.Hostname()
	}
	hostIP := os.Getenv("MY_NODE_IP")
	node_ip = hostIP
	node_name = hostname
	go c.CleanupExpiredInfo()

	// Call to Update() to start readers and set receivers once at the start.
	if err := c.Update(args); err != nil {
		return nil, err
	}

	return c, nil
}

// Run implements component.Component.
// TODO(@tpaschalis). Should we periodically re-check? What happens if a target
// comes alive _after_ it's been passed to us and we never receive another
// Update()? Or should it be a responsibility of the discovery component?
func (c *Component) Run(ctx context.Context) error {
	defer func() {
		level.Info(c.opts.Logger).Log("msg", "loki.source.file component shutting down, stopping readers and positions file")
		c.mut.RLock()
		for _, r := range c.readers {
			r.Stop()
		}
		c.posFile.Stop()
		close(c.handler.Chan())
		c.mut.RUnlock()
		c.stopch <- struct{}{}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case entry := <-c.handler.Chan():
			c.GetPromMetric(entry)
			c.mut.RLock()
			for _, receiver := range c.receivers {
				receiver.Chan() <- entry
			}
			c.mut.RUnlock()
		}
	}
}

// Update implements component.Component.
func (c *Component) Update(args component.Arguments) error {
	c.updateMut.Lock()
	defer c.updateMut.Unlock()

	// Stop all readers so we can recreate them below. This *must* be done before
	// c.mut is held to avoid a race condition where stopping a reader is
	// flushing its data, but the flush never succeeds because the Run goroutine
	// fails to get a read lock.
	//
	// Stopping the readers avoids the issue we saw with stranded wrapped
	// handlers staying behind until they were GC'ed and sending duplicate
	// message to the global handler. It also makes sure that we update
	// everything with the new labels. Simply zeroing out the c.readers map did
	// not work correctly to shut down the wrapped handlers in time.
	//
	// TODO (@tpaschalis) We should be able to optimize this somehow and eg.
	// cache readers for paths we already know about, and whose labels have not
	// changed. Once we do that we should:
	//
	// * Call to c.pruneStoppedReaders to give cached but errored readers a
	//   chance to restart.
	// * Stop tailing any files that were no longer in the new targets
	//   and conditionally remove their readers only by calling toStopTailing
	//   and c.stopTailingAndRemovePosition.
	oldPaths := c.stopReaders()

	newArgs := args.(Arguments)

	c.mut.Lock()
	defer c.mut.Unlock()
	c.args = newArgs
	c.receivers = newArgs.ForwardTo

	c.readers = make(map[positions.Entry]reader)

	if len(newArgs.Targets) == 0 {
		level.Debug(c.opts.Logger).Log("msg", "no files targets were passed, nothing will be tailed")
		return nil
	}

	for _, target := range newArgs.Targets {
		path := target[pathLabel]

		labels := make(model.LabelSet)
		for k, v := range target {
			if strings.HasPrefix(k, model.ReservedLabelPrefix) {
				continue
			}
			labels[model.LabelName(k)] = model.LabelValue(v)
		}

		// Deduplicate targets which have the same public label set.
		readersKey := positions.Entry{Path: path, Labels: labels.String()}
		if _, exist := c.readers[readersKey]; exist {
			continue
		}

		c.reportSize(path, labels.String())

		handler := loki.AddLabelsMiddleware(labels).Wrap(loki.NewEntryHandler(c.handler.Chan(), func() {}))
		reader, err := c.startTailing(path, labels, handler)
		if err != nil {
			continue
		}

		c.readers[readersKey] = readerWithHandler{
			reader:  reader,
			handler: handler,
		}
	}

	// Remove from the positions file any entries that had a Reader before, but
	// are no longer in the updated set of Targets.
	for r := range missing(c.readers, oldPaths) {
		c.posFile.Remove(r.Path, r.Labels)
	}

	return nil
}

// readerWithHandler combines a reader with an entry handler associated with
// it. Closing the reader will also close the handler.
type readerWithHandler struct {
	reader
	handler loki.EntryHandler
}

func (r readerWithHandler) Stop() {
	r.reader.Stop()
	r.handler.Stop()
}

// stopReaders stops existing readers and returns the set of paths which were
// stopped.
func (c *Component) stopReaders() map[positions.Entry]struct{} {
	c.mut.RLock()
	defer c.mut.RUnlock()

	stoppedPaths := make(map[positions.Entry]struct{}, len(c.readers))

	for p, r := range c.readers {
		stoppedPaths[p] = struct{}{}
		r.Stop()
	}

	return stoppedPaths
}

// DebugInfo returns information about the status of tailed targets.
// TODO(@tpaschalis) Decorate with more debug information once it's made
// available, such as the last time a log line was read.
func (c *Component) DebugInfo() interface{} {
	var res readerDebugInfo
	for e, reader := range c.readers {
		offset, _ := c.posFile.Get(e.Path, e.Labels)
		res.TargetsInfo = append(res.TargetsInfo, targetInfo{
			Path:       e.Path,
			Labels:     e.Labels,
			IsRunning:  reader.IsRunning(),
			ReadOffset: offset,
		})
	}
	return res
}

type readerDebugInfo struct {
	TargetsInfo []targetInfo `alloy:"targets_info,block"`
}

type targetInfo struct {
	Path       string `alloy:"path,attr"`
	Labels     string `alloy:"labels,attr"`
	IsRunning  bool   `alloy:"is_running,attr"`
	ReadOffset int64  `alloy:"read_offset,attr"`
}

// Returns the elements from set b which are missing from set a
func missing(as map[positions.Entry]reader, bs map[positions.Entry]struct{}) map[positions.Entry]struct{} {
	c := map[positions.Entry]struct{}{}
	for a := range bs {
		if _, ok := as[a]; !ok {
			c[a] = struct{}{}
		}
	}
	return c
}

// startTailing starts and returns a reader for the given path. For most files,
// this will be a tailer implementation. If the file suffix alludes to it being
// a compressed file, then a decompressor will be started instead.
func (c *Component) startTailing(path string, labels model.LabelSet, handler loki.EntryHandler) (reader, error) {
	fi, err := os.Stat(path)
	if err != nil {
		level.Error(c.opts.Logger).Log("msg", "failed to tail file, stat failed", "error", err, "filename", path)
		c.metrics.totalBytes.DeleteLabelValues(path)
		return nil, fmt.Errorf("failed to stat path %s", path)
	}

	if fi.IsDir() {
		level.Info(c.opts.Logger).Log("msg", "failed to tail file", "error", "file is a directory", "filename", path)
		c.metrics.totalBytes.DeleteLabelValues(path)
		return nil, fmt.Errorf("failed to tail file, it was a directory %s", path)
	}

	var reader reader
	if c.args.DecompressionConfig.Enabled {
		level.Debug(c.opts.Logger).Log("msg", "reading from compressed file", "filename", path)
		decompressor, err := newDecompressor(
			c.metrics,
			c.opts.Logger,
			handler,
			c.posFile,
			path,
			labels.String(),
			c.args.Encoding,
			c.args.DecompressionConfig,
		)
		if err != nil {
			level.Error(c.opts.Logger).Log("msg", "failed to start decompressor", "error", err, "filename", path)
			return nil, fmt.Errorf("failed to start decompressor %s", err)
		}
		reader = decompressor
	} else {
		level.Debug(c.opts.Logger).Log("msg", "tailing new file", "filename", path)
		pollOptions := watch.PollingFileWatcherOptions{
			MinPollFrequency: c.args.FileWatch.MinPollFrequency,
			MaxPollFrequency: c.args.FileWatch.MaxPollFrequency,
		}
		tailer, err := newTailer(
			c.metrics,
			c.opts.Logger,
			handler,
			c.posFile,
			path,
			labels.String(),
			c.args.Encoding,
			pollOptions,
			c.args.TailFromEnd,
		)
		if err != nil {
			level.Error(c.opts.Logger).Log("msg", "failed to start tailer", "error", err, "filename", path)
			return nil, fmt.Errorf("failed to start tailer %s", err)
		}
		reader = tailer
	}

	return reader, nil
}

func (c *Component) reportSize(path, labels string) {
	// Ask the reader to update the size if a reader exists, this keeps
	// position and size metrics in sync.
	if reader, ok := c.readers[positions.Entry{Path: path, Labels: labels}]; ok {
		err := reader.MarkPositionAndSize()
		if err != nil {
			level.Warn(c.opts.Logger).Log("msg", "failed to get file size from existing reader, ", "file", path, "error", err)
			return
		}
	} else {
		// Must be a new file, just directly read the size of it
		fi, err := os.Stat(path)
		if err != nil {
			return
		}
		c.metrics.totalBytes.WithLabelValues(path).Set(float64(fi.Size()))
	}
}

func (c *Component) GetPromMetric(entry loki.Entry) {
	content := entry.Entry.Line
	var svcTag = &ServiceTag{}
	svcTag.FillTag(entry.Labels)
	if parser.IsFirstLine(content) {
		lastLogInfo := c.GetLastLogInfo(svcTag)
		if lastLogInfo == nil {
			lastLogInfo = &LastLogInfo{
				isLastNewLine:                true,
				isFirstLineContainsTimestamp: parser.IsContainsTimestamp(content),
				pythonTraceback:              false,
				pythonTracebackExpected:      false,
				TimestampSecond:              entry.Entry.Timestamp.Unix(),
			}
			c.UpdateLogInfo(svcTag, lastLogInfo)
		}
		lastLogInfo.TimestampSecond = entry.Entry.Timestamp.Unix()

		if lastLogInfo.IsFirstLine(content) {
			lastLogInfo.isLastNewLine = true
			logLevel, exceptionType := parser.GuessLevelAndException(content)
			c.CounterInc(svcTag, logLevel, exceptionType)
		} else {
			lastLogInfo.isLastNewLine = false
		}
	}
}

func (c *Component) CounterInc(serviceTag *ServiceTag, logLevel parser.Level, exceptionType string) {
	c.metrics.loglevel.WithLabelValues(serviceTag.FileName, serviceTag.ServiceName, logLevel.String(), node_name, node_ip).Inc()
	if len(exceptionType) > 0 {
		c.metrics.logexception.WithLabelValues(serviceTag.FileName, serviceTag.ServiceName, exceptionType, node_name, node_ip).Inc()
	}
}

func (c *Component) GetLastLogInfo(serviceTag *ServiceTag) *LastLogInfo {
	ref, ok := c.lastLogInfo.Load(*serviceTag)
	if !ok {
		return nil
	}
	return ref.(*LastLogInfo)
}

func (c *Component) UpdateLogInfo(serviceTag *ServiceTag, info *LastLogInfo) {
	c.lastLogInfo.Store(*serviceTag, info)
}

func (c *Component) CleanupExpiredInfo() {
	ticker := time.NewTicker(5 * time.Minute)
	for {
		select {
		case <-c.stopch:
			ticker.Stop()
			return
		case <-ticker.C:
			now := time.Now().Unix()
			c.lastLogInfo.Range(func(key, value interface{}) bool {
				serviceTag := key.(ServiceTag)
				lastLogInfo := value.(*LastLogInfo)
				if lastLogInfo.TimestampSecond+30 < now {
					c.lastLogInfo.Delete(serviceTag)
				}
				return true
			})
		}
	}
}

type LastLogInfo struct {
	isLastNewLine                bool
	isFirstLineContainsTimestamp bool

	pythonTraceback         bool
	pythonTracebackExpected bool

	// 上次日志的时间戳
	TimestampSecond int64
}

type ServiceTag struct {
	ServiceName string
	FileName    string
}

func (s *ServiceTag) FillTag(labels model.LabelSet) {
	for key, value := range labels {
		switch key {
		case "app_name":
			s.ServiceName = string(value)
		case "filename":
			s.FileName = string(value)
		}
	}
}
func (s *ServiceTag) WithLogLevel(level parser.Level) api.MeasurementOption {
	return api.WithAttributeSet(attribute.NewSet(
		attribute.Key("level").String(level.String()),
		attribute.Key("service_name").String(s.ServiceName),
		attribute.Key("filename").String(s.FileName),
	))
}

func (s *ServiceTag) WithExceptionType(exceptionType string) api.MeasurementOption {
	return api.WithAttributeSet(attribute.NewSet(
		attribute.Key("exception").String(exceptionType),
		attribute.Key("service_name").String(s.ServiceName),
		attribute.Key("filename").String(s.FileName),
	))
}

func (l *LastLogInfo) IsFirstLine(content string) bool {
	// if l.isFirstLineContainsTimestamp {
	// 	return parser.IsContainsTimestamp(content)
	// }

	if strings.HasPrefix(content, "Traceback ") {
		l.pythonTraceback = true
		if l.pythonTracebackExpected {
			l.pythonTracebackExpected = false
			return false
		}
		return !l.isLastNewLine
	}
	if content == "The above exception was the direct cause of the following exception:" ||
		content == "During handling of the above exception, another exception occurred:" {
		l.pythonTracebackExpected = true
		return false
	}
	if l.pythonTraceback {
		l.pythonTraceback = false
		return false
	}

	return true
}
