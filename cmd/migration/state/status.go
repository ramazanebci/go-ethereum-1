package main

import (
	"fmt"
	"strings"
	"time"
)

type status struct {
	startAt       time.Time
	commitStartAt time.Time
	lastLogTime   time.Duration
	count         int
}

func newStatus() *status {
	return &status{startAt: time.Now(), lastLogTime: 30 * time.Second}
}

func (s *status) emitLog(force bool, prefix ...string) {
	s.count++
	if runtime := time.Since(s.startAt); runtime > s.lastLogTime || force {
		s.lastLogTime += 30 * time.Second
		fmt.Println(strings.Join(prefix, " "), "processing", s.count, "\trunning time", runtime)
	}
}

func (s *status) startDBCommit() {
	s.commitStartAt = time.Now()
}

func (s *status) emitCompleteLog(prefix ...string) {
	fmt.Println(strings.Join(prefix, " "), "complete", "processing", s.count, "\trunning time", time.Since(s.startAt), "\tcommit running time", time.Since(s.commitStartAt))
}
