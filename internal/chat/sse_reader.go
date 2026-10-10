package chat

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"
)

var dataPrefix = []byte("data:")
var eventPrefix = []byte("event:")

var sseBufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 64*1024)
		return &b
	},
}

// ReadSSE scans an SSE stream delivering (eventType, data) pairs.
func ReadSSE(r io.Reader, onEvent func(eventType, data string) error) error {
	bufPtr := sseBufferPool.Get().(*[]byte)
	defer sseBufferPool.Put(bufPtr)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(*bufPtr, 4*1024*1024)

	var (
		eventType string
		data      bytes.Buffer
	)

	for scanner.Scan() {
		line := scanner.Bytes()

		if len(line) == 0 {
			if data.Len() > 0 {
				if err := onEvent(eventType, data.String()); err != nil {
					return err
				}
				data.Reset()
				eventType = ""
			}
			continue
		}

		if after, ok := bytes.CutPrefix(line, eventPrefix); ok {
			ev := after
			eventType = strings.TrimSpace(string(ev))
			continue
		}

		if after, ok := bytes.CutPrefix(line, dataPrefix); ok {
			value := after
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(value)
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	if data.Len() > 0 {
		return onEvent(eventType, data.String())
	}

	return nil
}
