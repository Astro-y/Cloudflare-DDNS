//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func openPasswordPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Skip("PTY unavailable", err)
	}
	master := os.NewFile(uintptr(masterFD), "pty-master")
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slaveFD, err := unix.Open(fmt.Sprintf("/dev/pts/%d", number), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	slave := os.NewFile(uintptr(slaveFD), "pty-slave")
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

func awaitPasswordNoEcho(t *testing.T, slave *os.File) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		settings, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		if settings.Lflag&(unix.ECHO|unix.ECHONL) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("password reader did not disable echo")
}

func TestPasswordPTYHidesInputAndRestoresState(t *testing.T) {
	master, slave := openPasswordPTY(t)
	masterFD := int(master.Fd())
	if err := unix.SetNonblock(masterFD, true); err != nil {
		t.Fatal(err)
	}
	original, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type answer struct {
		password []byte
		err      error
	}
	result := make(chan answer, 1)
	go func() { p, e := readPasswordContext(ctx, slave); result <- answer{p, e} }()
	awaitPasswordNoEcho(t, slave)
	if _, err := unix.Write(masterFD, []byte("hidden-secret")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	var echo [100]byte
	if n, err := unix.Read(masterFD, echo[:]); n > 0 || !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("secret was echoed: n=%d err=%v", n, err)
	}
	if _, err := unix.Write(masterFD, []byte("\r")); err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil || string(got.password) != "hidden-secret" {
		t.Fatalf("password read failed: %v", got.err)
	}
	clear(got.password)
	after, err := term.GetState(int(slave.Fd()))
	if err != nil || !reflect.DeepEqual(original, after) {
		t.Fatal("terminal state was not restored", err)
	}
}

func TestPasswordPTYCancellationRestoresAndFlushes(t *testing.T) {
	master, slave := openPasswordPTY(t)
	original, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		p, e := readPasswordContext(ctx, slave)
		if len(p) != 0 {
			result <- errors.New("cancellation returned secret")
			return
		}
		result <- e
	}()
	awaitPasswordNoEcho(t, slave)
	if _, err := unix.Write(int(master.Fd()), []byte("unsubmitted-secret")); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
	after, err := term.GetState(int(slave.Fd()))
	if err != nil || !reflect.DeepEqual(original, after) {
		t.Fatal("cancel did not restore terminal", err)
	}
	// A subsequent canonical read must receive only the new line, with no secret.
	if _, err := unix.Write(int(master.Fd()), []byte("next\n")); err != nil {
		t.Fatal(err)
	}
	var next [100]byte
	n, err := unix.Read(int(slave.Fd()), next[:])
	if err != nil || string(next[:n]) != "next\n" {
		t.Fatalf("canceled secret remained in input: n=%d err=%v", n, err)
	}
}

func TestPasswordPTYEOFRestoresState(t *testing.T) {
	master, slave := openPasswordPTY(t)
	original, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, e := readPasswordContext(ctx, slave); result <- e }()
	awaitPasswordNoEcho(t, slave)
	if _, err := unix.Write(int(master.Fd()), []byte{4}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF: %v", err)
	}
	after, err := term.GetState(int(slave.Fd()))
	if err != nil || !reflect.DeepEqual(original, after) {
		t.Fatal("EOF did not restore terminal", err)
	}
}

func TestPasswordCanceledBeforeReading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readPasswordContext(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
