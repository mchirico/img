package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/containerd/containerd/platforms"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/console"
	"github.com/containerd/containerd/namespaces"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/pkg/archive"
	"github.com/mchirico/img/client"
	controlapi "github.com/moby/buildkit/api/services/control"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/appcontext"
	"github.com/moby/buildkit/util/progress/progressui"
	"golang.org/x/sync/errgroup"
)

const buildHelp = `Build an image from a Dockerfile.`

func (cmd *buildCommand) Name() string      { return "build" }
func (cmd *buildCommand) Args() string      { return "[OPTIONS] PATH" }
func (cmd *buildCommand) ShortHelp() string { return buildHelp }
func (cmd *buildCommand) LongHelp() string  { return buildHelp }
func (cmd *buildCommand) Hidden() bool      { return false }

func (cmd *buildCommand) Register(fs *flag.FlagSet) {
	fs.StringVar(&cmd.dockerfilePath, "file", "", "Name of the Dockerfile (Default is 'PATH/Dockerfile')")
	fs.StringVar(&cmd.dockerfilePath, "f", "", "Name of the Dockerfile (Default is 'PATH/Dockerfile')")
	fs.Var(&cmd.tags, "tag", "Name and optionally a tag in the 'name:tag' format")
	fs.Var(&cmd.tags, "t", "Name and optionally a tag in the 'name:tag' format")
	fs.StringVar(&cmd.target, "target", "", "Set the target build stage to build")
	fs.Var(&cmd.platforms, "platform", "Set platforms for which the image should be built")
	fs.Var(&cmd.buildArgs, "build-arg", "Set build-time variables")
	fs.Var(&cmd.labels, "label", "Set metadata for an image")
	fs.BoolVar(&cmd.noConsole, "no-console", false, "Use non-console progress UI")
	fs.BoolVar(&cmd.noCache, "no-cache", false, "Do not use cache when building the image")
}

type buildCommand struct {
	buildArgs      stringSlice
	dockerfilePath string
	labels         stringSlice
	target         string
	tags           stringSlice
	platforms      stringSlice

	contextDir string
	noConsole  bool
	noCache    bool
}

func (cmd *buildCommand) Run(ctx context.Context, args []string) (err error) {
	if len(args) < 1 {
		return fmt.Errorf("must pass a path to build")
	}

	if len(cmd.tags) < 1 {
		return errors.New("please specify an image tag with `-t`")
	}

	reexec()
	if err := installRuncIfDNE(); err != nil {
		return err
	}

	// Get the specified context.
	cmd.contextDir = args[0]

	// Parse what is set to come from stdin.
	if cmd.dockerfilePath == "-" {
		cmd.dockerfilePath, err = dockerfileFromStdin()
		if err != nil {
			return fmt.Errorf("reading dockerfile from stdin failed: %v", err)
		}
		// On exit cleanup the temporary file we used hold the dockerfile from stdin.
		defer os.RemoveAll(cmd.dockerfilePath)
	}

	if cmd.contextDir == "" {
		return errors.New("please specify build context (e.g. \".\" for the current directory)")
	}

	if cmd.contextDir == "-" {
		cmd.contextDir, err = contextFromStdin(cmd.dockerfilePath)
		if err != nil {
			return fmt.Errorf("reading context from stdin failed: %v", err)
		}
		// On exit cleanup the temporary directory we used hold the files from stdin.
		defer os.RemoveAll(cmd.contextDir)
	}

	for position, tag := range cmd.tags {
		// Parse the image name and tag.
		named, err := reference.ParseNormalizedNamed(tag)
		if err != nil {
			return fmt.Errorf("parsing image name %q failed: %v", tag, err)
		}
		// Add the latest tag if they did not provide one.
		named = reference.TagNameOnly(named)
		cmd.tags[position] = named.String()
	}

	initialTag := cmd.tags[0]

	// Set the dockerfile path as the default if one was not given.
	if cmd.dockerfilePath == "" {
		cmd.dockerfilePath, err = securejoin.SecureJoin(cmd.contextDir, defaultDockerfileName)
		if err != nil {
			return err
		}
	}

	if len(cmd.platforms) < 1 {
		cmd.platforms = []string{platforms.DefaultString()}
	}
	platforms := strings.Join(cmd.platforms, ",")

	// Create the client.
	c, err := client.New(stateDir, backend, cmd.getLocalDirs())
	if err != nil {
		return err
	}
	defer c.Close()

	// Create the frontend attrs.
	frontendAttrs := map[string]string{
		// We use the base for filename here because we already set up the local dirs which sets the path in createController.
		"filename": filepath.Base(cmd.dockerfilePath),
		"target":   cmd.target,
		"platform": platforms,
	}
	if cmd.noCache {
		frontendAttrs["no-cache"] = ""
	}

	// Get the build args and add them to frontend attrs.
	for _, buildArg := range cmd.buildArgs {
		kv := strings.SplitN(buildArg, "=", 2)
		if len(kv) != 2 {
			return fmt.Errorf("invalid build-arg value %s", buildArg)
		}
		frontendAttrs["build-arg:"+kv[0]] = kv[1]
	}

	for _, label := range cmd.labels {
		kv := strings.SplitN(label, "=", 2)
		if len(kv) != 2 {
			return fmt.Errorf("invalid label value %s", label)
		}
		frontendAttrs["label:"+kv[0]] = kv[1]
	}

	fmt.Printf("Building %s\n", initialTag)
	fmt.Println("Setting up the rootfs... this may take a bit.")

	// Create the context.
	ctx = appcontext.Context()
	sess, sessDialer, err := c.Session(ctx)
	if err != nil {
		return err
	}
	id := identity.NewID()
	ctx = session.NewContext(ctx, sess.ID())
	ctx = namespaces.WithNamespace(ctx, "buildkit")
	eg, ctx := errgroup.WithContext(ctx)

	ch := make(chan *controlapi.StatusResponse)
	eg.Go(func() error {
		return sess.Run(ctx, sessDialer)
	})
	// Solve the dockerfile.
	eg.Go(func() error {
		defer sess.Close()
		return c.Solve(ctx, &controlapi.SolveRequest{
			Ref:      id,
			Session:  sess.ID(),
			Exporter: "image",
			ExporterAttrs: map[string]string{
				"name": strings.Join(cmd.tags, ","),
			},
			Frontend:      "dockerfile.v0",
			FrontendAttrs: frontendAttrs,
		}, ch)
	})
	eg.Go(func() error {
		return showProgress(ch, cmd.noConsole)
	})
	if err := eg.Wait(); err != nil {
		return err
	}
	fmt.Printf("Successfully built %s\n", initialTag)

	return nil
}

// dockerfileFromStdin copies a dockerfile from stdin to a temporary file.
func dockerfileFromStdin() (string, error) {
	stdin, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("reading from stdin failed: %v", err)
	}

	// Create a temporary file for the Dockerfile
	f, err := ioutil.TempFile("", "img-build-dockerfile-")
	if err != nil {
		return f.Name(), fmt.Errorf("unable to create temporary file for dockerfile: %v", err)
	}
	defer f.Close()

	if _, err := f.Write(stdin); err != nil {
		return f.Name(), fmt.Errorf("writing to temporary file for dockerfile failed: %v", err)
	}

	return f.Name(), nil
}

// contextFromStdin will read the contents of stdin as either a
// Dockerfile or tar archive. Returns the path to a temporary directory
// for the build context..
func contextFromStdin(dockerfileName string) (string, error) {
	// Set the dockerfile name if it is empty.
	if dockerfileName == "" {
		dockerfileName = defaultDockerfileName
	}

	// Create a temporary directory for the build context.
	tmpDir, err := ioutil.TempDir("", "img-build-context-")
	if err != nil {
		return "", fmt.Errorf("unable to create temporary context directory: %v", err)
	}

	// Create a new reader from stdin.
	buf := bufio.NewReader(os.Stdin)

	// Grab the magic number range from the reader.
	archiveHeaderSize := 512 // number of bytes in an archive header
	magic, err := buf.Peek(archiveHeaderSize)
	if err != nil && err != io.EOF {
		return tmpDir, fmt.Errorf("failed to peek context header from STDIN: %v", err)
	}

	// Validate if it is a tar archive.
	if isArchive(magic) {
		return tmpDir, untar(tmpDir, buf)
	}

	if dockerfileName == "-" {
		return tmpDir, errors.New("build context is not an archive")
	}

	// Create the dockerfile in the temporary directory.
	dockerfilePath, err := securejoin.SecureJoin(tmpDir, dockerfileName)
	if err != nil {
		return tmpDir, err
	}
	f, err := os.Create(dockerfilePath)
	if err != nil {
		return tmpDir, err
	}
	defer f.Close()

	// Copy the contents of the reader to the file.
	_, err = io.Copy(f, buf)
	return tmpDir, err
}

// isArchive checks for the magic bytes of a tar or any supported compression algorithm.
func isArchive(header []byte) bool {
	compression := archive.DetectCompression(header)
	if compression != archive.Uncompressed {
		return true
	}
	r := tar.NewReader(bytes.NewBuffer(header))
	_, err := r.Next()
	return err == nil
}

// untar unpacks a tarball to a given directory.
func untar(dest string, r io.Reader) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		switch {
		// if no more files are found return
		case err == io.EOF:
			return nil
		// return any other error
		case err != nil:
			return err
		// if the header is nil, just skip it (not sure how this happens)
		case header == nil:
			continue
		}

		// the target location where the dir/file should be created
		target, err := securejoin.SecureJoin(dest, header.Name)
		if err != nil {
			return err
		}

		// check the file type
		switch header.Typeflag {
		// if its a dir and it doesn't exist create it
		case tar.TypeDir:
			if _, err := os.Stat(target); err != nil {
				if err := os.MkdirAll(target, 0755); err != nil {
					return err
				}
			}
		// if it's a file create it
		case tar.TypeReg:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			// copy over contents
			_, err = io.Copy(f, tr)
			// immediately close the file, as opposed to doing it in a defer.
			// This is so we don't leak open files.
			f.Close()
			if err != nil {
				return err
			}
		}
	}
}

func (cmd *buildCommand) getLocalDirs() map[string]string {
	return map[string]string{
		"context":    cmd.contextDir,
		"dockerfile": filepath.Dir(cmd.dockerfilePath),
	}
}

func showProgress(ch chan *controlapi.StatusResponse, noConsole bool) error {
	displayCh := make(chan *bkclient.SolveStatus)
	go func() {
		for resp := range ch {
			s := bkclient.SolveStatus{}
			for _, v := range resp.Vertexes {
				s.Vertexes = append(s.Vertexes, &bkclient.Vertex{
					Digest:    v.Digest,
					Inputs:    v.Inputs,
					Name:      v.Name,
					Started:   v.Started,
					Completed: v.Completed,
					Error:     v.Error,
					Cached:    v.Cached,
				})
			}
			for _, v := range resp.Statuses {
				s.Statuses = append(s.Statuses, &bkclient.VertexStatus{
					ID:        v.ID,
					Vertex:    v.Vertex,
					Name:      v.Name,
					Total:     v.Total,
					Current:   v.Current,
					Timestamp: v.Timestamp,
					Started:   v.Started,
					Completed: v.Completed,
				})
			}
			for _, v := range resp.Logs {
				s.Logs = append(s.Logs, &bkclient.VertexLog{
					Vertex:    v.Vertex,
					Stream:    int(v.Stream),
					Data:      v.Msg,
					Timestamp: v.Timestamp,
				})
			}
			displayCh <- &s
		}
		close(displayCh)
	}()
	var c console.Console
	if !noConsole {
		if cf, err := console.ConsoleFromFile(os.Stderr); err == nil {
			c = cf
		}
	}
	return progressui.DisplaySolveStatus(context.TODO(), "", c, os.Stdout, displayCh)
}
