package logging

import (
	"io"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
)

// Init initializes the logger based on configuration
func Init(level, output string) (*logrus.Logger, error) {
	logger := logrus.New()

	// Set log level
	logLevel, err := logrus.ParseLevel(level)
	if err != nil {
		logLevel = logrus.InfoLevel
	}
	logger.SetLevel(logLevel)

	// Set log format
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
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
