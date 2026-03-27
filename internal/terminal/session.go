package terminal

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

type Session struct {
	cmd      *exec.Cmd
	ptyFile  *os.File
	onOutput func([]byte)
	onExit   func(error)

	writeMu sync.Mutex
	oneStop sync.Once
}

func NewSession(shellPath, workDir string, onOutput func([]byte), onExit func(error)) (*Session, error) {
	cmd := exec.Command(shellPath)
	cmd.Dir = workDir
	cmd.Env = append(
		os.Environ(),
		"TERM=xterm-256color",
		"NO_COLOR=1",
		"CLICOLOR=0",
		"FORCE_COLOR=0",
	)

	ptyFile, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}

	s := &Session{
		cmd:      cmd,
		ptyFile:  ptyFile,
		onOutput: onOutput,
		onExit:   onExit,
	}

	go s.readLoop()
	go s.waitLoop()

	return s, nil
}

func (s *Session) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptyFile.Read(buf)
		if n > 0 && s.onOutput != nil {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.onOutput(chunk)
		}
		if err != nil {
			if err != io.EOF {
				s.Stop()
			}
			return
		}
	}
}

func (s *Session) waitLoop() {
	err := s.cmd.Wait()
	s.Stop()
	if s.onExit != nil {
		s.onExit(err)
	}
}

func (s *Session) WriteLine(input string) error {
	return s.WriteRaw(input + "\n")
}

func (s *Session) WriteRaw(input string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err := s.ptyFile.WriteString(input)
	return err
}

func (s *Session) WriteBytes(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err := s.ptyFile.Write(data)
	return err
}

func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid terminal size cols=%d rows=%d", cols, rows)
	}
	return pty.Setsize(s.ptyFile, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (s *Session) Stop() {
	s.oneStop.Do(func() {
		_ = s.ptyFile.Close()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
}
