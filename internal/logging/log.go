package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
)

const defaultTimestampFormat = "2006-01-02 15:04:05"

// BracketFormatter renders logs as:
// [timestamp] [LEVEL] [file:line] message
type BracketFormatter struct {
	TimestampFormat string
}

func (f *BracketFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	timestampFormat := f.TimestampFormat
	if timestampFormat == "" {
		timestampFormat = defaultTimestampFormat
	}

	caller := "unknown:0"
	if entry.Caller != nil {
		caller = fmt.Sprintf("%s:%d", shortenCallerPath(entry.Caller.File), entry.Caller.Line)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "[%s] [%s] [%s] %s", entry.Time.Format(timestampFormat), strings.ToUpper(entry.Level.String()), caller, entry.Message)

	if len(entry.Data) > 0 {
		keys := make([]string, 0, len(entry.Data))
		for key := range entry.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&sb, " %s=%v", key, entry.Data[key])
		}
	}

	sb.WriteByte('\n')
	return []byte(sb.String()), nil
}

func shortenCallerPath(file string) string {
	normalized := filepath.ToSlash(file)
	for _, marker := range []string{"/internal/", "/cmd/"} {
		if idx := strings.Index(normalized, marker); idx >= 0 {
			return normalized[idx+1:]
		}
	}
	return filepath.Base(normalized)
}

// Init initializes the logger based on configuration
func Init(level, output string) (*logrus.Logger, error) {
	logger := logrus.New()

	// Set log level
	logLevel, err := logrus.ParseLevel(level)
	if err != nil {
		logLevel = logrus.InfoLevel
	}
	logger.SetLevel(logLevel)
	logger.SetReportCaller(true)

	// Set log format
	logger.SetFormatter(&BracketFormatter{
		TimestampFormat: defaultTimestampFormat,
	})

	// Set output
	var writers []io.Writer
	writers = append(writers, os.Stdout)

	if output != "" && output != "stdout" {
		// Ensure directory exists
		dir := filepath.Dir(output)
		if dir != "." && dir != ".." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, err
			}
		}

		file, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			return nil, err
		}
		writers = append(writers, file)
	}

	logger.SetOutput(io.MultiWriter(writers...))

	return logger, nil
}
