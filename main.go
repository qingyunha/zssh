package main

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/peterh/liner"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func main() {
	log.SetFlags(0)
	args := append([]string{"-e", "none"}, os.Args[1:]...)
	ssh := exec.Command("ssh", args...)
	ptmx, err := pty.Start(ssh)
	if err != nil {
		log.Println("start pty error:", err)
		return
	}
	defer func() { _ = ptmx.Close() }()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	if err := pty.InheritSize(os.Stdin, ptmx); err != nil {
		log.Printf("error resizing pty: %s", err)
	}
	go func() {
		for range ch {
			if err := pty.InheritSize(os.Stdin, ptmx); err != nil {
				log.Printf("error resizing pty: %s", err)
			}
		}
	}()
	defer func() { signal.Stop(ch); close(ch) }()

	oldState, err := term.MakeRaw(0)
	if err != nil {
		log.Fatal("term make raw error:", err)
	}
	defer func() { _ = term.Restore(0, oldState) }()

	// reopen stdout, keep it in block mode
	// otherwise write will return EAGIN, e.g. cat a large file
	os.Stdout.Close()
	os.Stdout, err = os.OpenFile("/dev/tty", os.O_WRONLY, 0666)
	if err != nil {
		panic(err)
	}
	c := newCopyStdin(ptmx)

	if err := rzsz(ptmx, c); err != nil {
		log.Println(err)
	}
}

func rzsz(ptmx *os.File, c *copyStdin) error {
	var buf [512]byte
	const PASSWORD_MATCH = "assword"
	const MAX_PASSWORD_LINE = 3
	passwd := os.Getenv("ZSSH_PASSWORD")
	os.Unsetenv("ZSSH_PASSWORD") // not realy work
	readCont := 0
	if passwd == "" {
		readCont = MAX_PASSWORD_LINE
	}
	for {
		n, err := ptmx.Read(buf[:])
		if err == io.EOF || errors.Is(err, syscall.EIO) {
			break
		}
		if err != nil {
			return err
		}
		if bytes.Contains(buf[:n], []byte("**\x18B00000000000000")) {
			dorz(ptmx, buf[:n], c)
			continue
		}
		if bytes.Contains(buf[:n], []byte("**\x18B0100000023be50")) {
			dosz(ptmx, buf[:n], c)
			continue
		}
		if nn, err := os.Stdout.Write(buf[:n]); err != nil || nn != n {
			panic(err)
		}
		if readCont < MAX_PASSWORD_LINE {
			readCont += 1
			if bytes.Contains(buf[:n], []byte(PASSWORD_MATCH)) {
				ptmx.Write([]byte(passwd))
				ptmx.Write([]byte{'\n'})
			}
		}
	}
	return nil
}

func dorz(ptmx *os.File, start []byte, c *copyStdin) {
	c.cancel()
	defer c.restart()
	defer term.MakeRaw(0)
	os.Stdout.Write([]byte{'\r'})
	dir, err := selectDir()
	if err != nil {
		log.Printf("select Dir error: %v", err)
		ptmx.Write([]byte{0x18, 0x18, 0x18, 0x18, 0x18})
		return
	}
	cmd := exec.Command("rz", "--rename", "--binary")
	cmd.Dir = dir
	cmd.Stdout = ptmx
	cmd.Stderr = os.Stdout
	cmd.Stdin = ptmx
	//cmd.Stdin = &fWithStart{ptmx, start}
	if err := cmd.Run(); err != nil {
		ptmx.Write([]byte{0x18, 0x18, 0x18, 0x18, 0x18})
		log.Printf("run rz error: %v", err)
	}
	ptmx.Write([]byte{'\n'})

}

func dosz(ptmx *os.File, start []byte, c *copyStdin) {
	c.cancel()
	defer c.restart()
	defer term.MakeRaw(0)
	os.Stdout.Write([]byte{'\r'})
	file, err := selectFile()
	if err != nil {
		log.Printf("select file error: %v", err)
		ptmx.Write([]byte{0x18, 0x18, 0x18, 0x18, 0x18})
		return
	}
	cmd := exec.Command("sz", file, "--binary")
	cmd.Stdout = ptmx
	cmd.Stderr = os.Stdout
	cmd.Stdin = &fWithStart{ptmx, start}
	//cmd.Stdin = ptmx
	if err := cmd.Run(); err != nil {
		ptmx.Write([]byte{0x18, 0x18, 0x18, 0x18, 0x18})
		log.Printf("run rz error: %v", err)
	}
	ptmx.Write([]byte{'\n'})

}

func selectFile() (string, error) {
	line := liner.NewLiner()
	defer line.Close()
	line.SetCompleter(func(line string) (c []string) {
		if len(line) > 0 && line[len(line)-1] != '/' {
			if info, err := os.Stat(line); err == nil {
				if info.IsDir() {
					line += "/"
				}
			}
		}
		dir, file := path.Split(line)
		if dir == "" {
			dir = "."
		}
		fs, err := ioutil.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range fs {
			name := e.Name()
			if strings.HasPrefix(name, file) {
				c = append(c, filepath.Join(dir, name))
			}
		}
		return
	})
	name, err := line.Prompt("select a file to send: ")
	return name, err
}

func selectDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	line := liner.NewLiner()
	defer line.Close()
	line.SetCompleter(func(line string) (c []string) {
		dir, file := path.Split(line)
		if dir == "" {
			dir = "."
		}
		fs, err := ioutil.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range fs {
			name := e.Name()
			if strings.HasPrefix(name, file) && e.IsDir() {
				c = append(c, filepath.Join(dir, name)+"/")
			}
		}
		return
	})
	name, err := line.PromptWithSuggestion("select directory to recive file: ", cwd, -1)
	return name, err
}

type fWithStart struct {
	f     *os.File
	start []byte
}

func (f *fWithStart) Read(p []byte) (n int, err error) {
	if len(f.start) == 0 {
		return f.f.Read(p)
	}
	n = copy(p, f.start)
	f.start = nil
	return

}

type copyStdin struct {
	startc chan struct{}
	src    *fdCancel
	dst    *os.File
}

func newCopyStdin(dst *os.File) *copyStdin {
	src, err := newfdCancel(0)
	if err != nil {
		panic(err)
	}
	c := &copyStdin{
		make(chan struct{}),
		src,
		dst,
	}
	go func() {
		for {
			io.Copy(c.dst, src)
			<-c.startc
			c.src.setNonblock(true)
		}
	}()
	return c
}

func (c *copyStdin) restart() {
	select {
	case c.startc <- struct{}{}:
	default:
	}
}

func (c *copyStdin) cancel() {
	c.src.cancel()
	c.src.setNonblock(false)
	time.Sleep(10 * time.Millisecond)
}

type fdCancel struct {
	fd            int
	closingReader *os.File
	closingWriter *os.File
}

func newfdCancel(fd int) (*fdCancel, error) {
	err := unix.SetNonblock(fd, true)
	if err != nil {
		return nil, err
	}
	fdccancel := fdCancel{fd: fd}

	fdccancel.closingReader, fdccancel.closingWriter, err = os.Pipe()
	if err != nil {
		return nil, err
	}

	return &fdccancel, nil
}

func retryAfterError(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR)
}

func (fdc *fdCancel) readyRead() bool {
	closeFd := int32(fdc.closingReader.Fd())

	pollFds := []unix.PollFd{{Fd: int32(fdc.fd), Events: unix.POLLIN}, {Fd: closeFd, Events: unix.POLLIN}}
	var err error
	for {
		_, err = unix.Poll(pollFds, -1)
		if err == nil || !retryAfterError(err) {
			break
		}
	}
	if err != nil {
		return false
	}
	if pollFds[1].Revents != 0 {
		var p [1]byte
		unix.Read(int(closeFd), p[:])
		return false
	}
	return pollFds[0].Revents != 0
}

func (fdc *fdCancel) Read(p []byte) (n int, err error) {
	for {
		n, err := unix.Read(fdc.fd, p)
		if err == nil || !retryAfterError(err) {
			return n, err
		}
		if !fdc.readyRead() {
			return 0, os.ErrClosed
		}
	}
}

func (fdc *fdCancel) cancel() (err error) {
	_, err = fdc.closingWriter.Write([]byte{0})
	return
}

func (fdc *fdCancel) close() {
	fdc.closingReader.Close()
	fdc.closingWriter.Close()
}

func (fdc *fdCancel) setNonblock(nonblock bool) (err error) {
	return unix.SetNonblock(fdc.fd, nonblock)
}
