package exec

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "-zsync-pipe-send":
		os.Stdout.Write(bytes.Repeat([]byte("a"), 1234))
		os.Exit(0)
	case len(os.Args) > 1 && os.Args[1] == "-zsync-pipe-recv":
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRunPipeCountsBytes(t *testing.T) {
	local := &LocalExecutor{}
	n, err := local.RunPipe(context.Background(), local,
		os.Args[0], []string{"-zsync-pipe-send"},
		os.Args[0], []string{"-zsync-pipe-recv"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1234 {
		t.Fatalf("bytes = %d", n)
	}
}
