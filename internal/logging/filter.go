package logging

import (
	"fmt"
	"log"
	"strings"
	"sync"
)

type Filter struct {
	mu     sync.RWMutex
	logger *log.Logger
	level  int
}

func New(logger *log.Logger, level string) *Filter {
	filter := &Filter{logger: logger}
	filter.SetLevel(level)
	return filter
}

func (f *Filter) SetLevel(level string) {
	threshold := 0
	switch strings.ToLower(level) {
	case "debug":
		threshold = -1
	case "warn":
		threshold = 1
	case "error":
		threshold = 2
	}
	f.mu.Lock()
	f.level = threshold
	f.mu.Unlock()
}

func (f *Filter) Printf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	severity := messageSeverity(message)
	f.mu.RLock()
	logger, threshold := f.logger, f.level
	f.mu.RUnlock()
	if logger != nil && severity >= threshold {
		logger.Print(message)
	}
}

func messageSeverity(message string) int {
	first := strings.ToUpper(strings.TrimSpace(strings.SplitN(message, " ", 2)[0]))
	switch first {
	case "DEBUG":
		return -1
	case "WARN", "WARNING":
		return 1
	case "ERROR":
		return 2
	default:
		return 0
	}
}
