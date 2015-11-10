package bake

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	// ErrDependency is set on a build if a dependent build has an error.
	ErrDependency = errors.New("dependency error")

	// ErrCanceled is set on a build if the build was canceled early.
	ErrCanceled = errors.New("build canceled")
)

// Builder processes targets to produce an output Build.
type Builder struct {
	mu     sync.Mutex
	builds map[*Build]struct{}

	wg      sync.WaitGroup
	closing chan struct{}

	// Used for tracking read/write access during build steps.
	FileSystem FileSystem

	Output io.Writer
}

// NewBuilder returns a new instance of Builder.
func NewBuilder() *Builder {
	return &Builder{
		builds:  make(map[*Build]struct{}),
		closing: make(chan struct{}),

		FileSystem: &nopFileSystem{},
		Output:     ioutil.Discard,
	}
}

// Build recursively executes build steps in parallel.
func (b *Builder) Build(build *Build) {
	b.build(build)

	// Notify all goroutines to clean up.
	// Open builds can still be going if an error occurred and bubbled up.
	close(b.closing)
	b.wg.Wait()
}

// buildEach executes a list of builds in parallel.
func (b *Builder) buildEach(builds []*Build) error {
	results := make(chan *Build)

	// Build dependencies in order.
	for _, build := range builds {
		go func(build *Build) {
			b.build(build)
			results <- build
		}(build)
	}

	// Wait for all dependencies to finish.
	// If an error occurs on any then mark this build as errored and bubble up.
	// Unfinished subbuilds will be signaled when the builder broadcasts on
	// the closing channel.
	var n int
	for {
		if n == len(builds) {
			break
		}

		select {
		case build := <-results:
			if err := build.Err(); err != nil {
				return ErrDependency
			}
		case <-b.closing:
			return ErrCanceled
		}

		n++
	}

	return nil
}

// build processes a single build and its dependencies.
func (b *Builder) build(build *Build) {
	// Ensure that only one build goroutine executes. All others should wait.
	if !b.reserve(build) {
		build.Wait()
		return
	}

	// Build all depedencies first.
	if err := b.buildEach(build.Dependencies()); err != nil {
		build.Done(err)
		return
	}

	time.Sleep(time.Duration(rand.Intn(int(1 * time.Second))))

	// Execute build after dependencies are finished.
	target := build.Target()
	if target != nil {
		fmt.Printf("BUILD: %s\n", target.Name)
		for _, cmd := range target.Commands {
			if err := b.run(build, cmd); err != nil {
				build.Done(err)
				return
			}
		}
		fmt.Println("")
	}

	// Mark build as finished with no error.
	build.Done(nil)
}

// reserve obtains the exclusive right to execute a build.
// Returns true if the caller should then process the build.
// Returns false if another caller has already obtained the right to build.
func (b *Builder) reserve(build *Build) (reserved bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Build is already in process.
	if _, ok := b.builds[build]; ok {
		return false
	}

	// Mark build as reserved.
	b.builds[build] = struct{}{}
	return true
}

// runs executes a command.
func (b *Builder) run(build *Build, cmd Command) error {
	switch cmd := cmd.(type) {
	case *ExecCommand:
		return b.runExec(build, cmd)
	default:
		panic(fmt.Sprintf("invalid command type: %T", cmd))
	}
}

// runExec runs an "exec" command against the shell.
func (b *Builder) runExec(build *Build, cmd *ExecCommand) error {
	fmt.Printf("  %s\n", strings.Join(cmd.Args, " "))

	c := exec.Command(cmd.Args[0], cmd.Args[1:]...)
	c.Dir = build.target.WorkDir
	c.Stdout = build.stdout.writer
	c.Stderr = build.stderr.writer
	return c.Run()
}
