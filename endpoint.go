package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// endpoint dispatches `aq endpoint <sub>` — the service-shaped half of the
// Jobs vocabulary (an image with a port, called over HTTP), kept as its own
// top-level GROUP rather than folded into `aq job create`'s already-large
// flag surface: the two shapes answer different questions ("run this once"
// vs. "call this URL"), the console splits them the same way (/jobs vs.
// /endpoints, two separate wizards), and `aq job create` refuses --port by
// name rather than silently meaning something different depending on which
// flags come with it.
func endpoint(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aq endpoint <create|list|url> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return endpointCreate(rest)
	case "list":
		return endpointList(rest)
	case "url":
		return endpointURL(rest)
	default:
		return fmt.Errorf("aq endpoint: unknown subcommand %q, expected one of create, list, url", sub)
	}
}

// endpointCreateOptions configures runEndpointCreate. endpointCreate() fills
// in the real environment; tests run runEndpointCreate directly.
type endpointCreateOptions struct {
	cred         *config.Credential
	image        string
	port         int
	path         string
	gpuModels    []string
	diskGB       int
	maxInstances int
	keepWarm     bool
	name         string
	out          io.Writer
}

// endpointCreateFlags is every flag `aq endpoint create` accepts, registered
// in one place so the top-level `aq --help` block can be checked against the
// real flag set (job_help_test.go's boundary-match guard, aq#91) rather than
// against a second copy someone remembered to update.
type endpointCreateFlags struct {
	name         *string
	image        *string
	port         *int
	path         *string
	gpuModels    *stringList
	diskGB       *int
	maxInstances *int
	keepWarm     *bool
}

func registerEndpointCreateFlags(fs *flag.FlagSet) *endpointCreateFlags {
	f := &endpointCreateFlags{gpuModels: &stringList{}}
	f.name = fs.String("name", "", "endpoint name (default: derived from the image ref)")
	f.image = fs.String("image", "", "a public or private image ref (required)")
	f.port = fs.Int("port", 0, "port inside the box the image's own server listens on (required)")
	f.path = fs.String("path", "/", "path this endpoint's caller POSTs to")
	fs.Var(f.gpuModels, "gpu-model", "exact marketplace GPU model name (see `aq gpus`) this endpoint may run on (repeatable; required)")
	f.diskGB = fs.Int("disk-gb", 100, "disk size in GB (default: 100, matching the console's Endpoints form)")
	f.maxInstances = fs.Int("max-instances", 0, "maximum concurrent instances this endpoint may run (required)")
	f.keepWarm = fs.Bool("keep-warm", false, "keep one instance running between calls instead of scaling to zero (sends minInstances: 1)")
	return f
}

// endpointCreate parses `aq endpoint create --image <ref> --port <p> ...`
// and wires the real environment into runEndpointCreate.
//
// --max-instances is required for the same reason `aq job create`'s is: an
// endpoint hands out a GPU budget and never defaults to unbounded.
func endpointCreate(args []string) error {
	fs := flag.NewFlagSet("endpoint create", flag.ContinueOnError)
	f := registerEndpointCreateFlags(fs)
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}

	image := strings.TrimSpace(*f.image)
	if image == "" {
		return errors.New("usage: aq endpoint create --image <ref> --port <p> --gpu-model <name> --max-instances <n> [--path /] [--keep-warm] [--name <n>]")
	}
	if *f.port <= 0 || *f.port >= 65536 {
		return errors.New("--port is required and must be between 1 and 65535")
	}
	path := *f.path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("--path must be an absolute path starting with /, got %q", path)
	}
	gpuModels := []string(*f.gpuModels)
	if len(gpuModels) == 0 {
		return errors.New("--gpu-model is required (see `aq gpus` for exact names); repeat the flag to allow more than one")
	}
	// Same bound as `aq job create --disk-gb` (job.go): matches the
	// console's own <Input type="number" min={10}> on the Endpoints form's
	// Advanced panel, with the same upper bound job create already enforces.
	if *f.diskGB < 10 || *f.diskGB > 10_000 {
		return fmt.Errorf("--disk-gb must be between 10 and 10000, got %d", *f.diskGB)
	}
	if *f.maxInstances <= 0 {
		return errors.New("--max-instances is required and must be a positive number: an endpoint hands out a GPU budget, so it never defaults to unbounded")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runEndpointCreate(endpointCreateOptions{
		cred:         cred,
		image:        image,
		port:         *f.port,
		path:         path,
		gpuModels:    gpuModels,
		diskGB:       *f.diskGB,
		maxInstances: *f.maxInstances,
		keepWarm:     *f.keepWarm,
		name:         *f.name,
		out:          os.Stdout,
	})
}

// endpointDerivedName mirrors console/app/endpoints/new/page.tsx's own
// `derivedName` for an image source: the last path segment of the ref, tag
// stripped, "new-endpoint" when that yields nothing usable. Deliberately its
// own function rather than reusing job.go's imageDerivedName, which falls
// back to "new-job" — the two commands need different fallback text even
// though the parsing rule is identical.
func endpointDerivedName(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "new-endpoint"
	}
	last := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		last = ref[i+1:]
	}
	if i := strings.Index(last, ":"); i >= 0 {
		last = last[:i]
	}
	if last == "" {
		return "new-endpoint"
	}
	return last
}

// runEndpointCreate builds the CreateEndpointRequest and posts it. The wire
// shape is fixed, matching what the console's own endpoints/new page sends
// and what the orchestrator's entrypoint parser requires for an http
// entrypoint: `entrypoint: {kind:"http", port, path, method:"POST",
// resultMode:"inline"}`, `minInstances: 1` iff --keep-warm was passed,
// omitted otherwise.
func runEndpointCreate(opts endpointCreateOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)

	name := strings.TrimSpace(opts.name)
	if name == "" {
		name = endpointDerivedName(opts.image)
	}

	req := api.CreateEndpointRequest{
		Name:  name,
		Image: &api.ImageSource{Ref: opts.image},
		Entrypoint: &api.HttpEntrypoint{
			Kind:       "http",
			Port:       opts.port,
			Path:       opts.path,
			Method:     "POST",
			ResultMode: "inline",
		},
		Hardware: &api.Hardware{
			GPUModels: opts.gpuModels,
			GPUCount:  1,
			DiskGB:    opts.diskGB,
		},
		MaxInstances: opts.maxInstances,
	}
	if opts.keepWarm {
		req.MinInstances = 1
	}

	ep, err := client.CreateEndpoint(req)
	if err != nil {
		return fmt.Errorf("could not create endpoint %q: %w", name, err)
	}

	fmt.Fprintf(out, "✓ Created endpoint %q → image %s, port %d (max %d instance(s))\n",
		ep.Name, opts.image, opts.port, opts.maxInstances)
	if ep.RunURL != nil && *ep.RunURL != "" {
		fmt.Fprintf(out, "  %s\n", *ep.RunURL)
	}
	return nil
}

// endpointList parses `aq endpoint list` and prints the team's endpoints —
// GET /jobs?shape=service, never the unfiltered list: mixing batch jobs onto
// this page would defeat the entire point of the shape split (jobs spec
// "Verified system facts" 1).
func endpointList(args []string) error {
	fs := flag.NewFlagSet("endpoint list", flag.ContinueOnError)
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	client := newControlClient(cred)
	list, err := client.ListJobsByShape("service")
	if err != nil {
		return fmt.Errorf("could not list endpoints: %w", err)
	}

	printEndpoints(os.Stdout, list)
	return nil
}

// printEndpoints renders the endpoint list as a simple aligned table.
func printEndpoints(out io.Writer, list []api.Job) {
	if len(list) == 0 {
		fmt.Fprintln(out, "No endpoints yet. Run `aq endpoint create` to make one.")
		return
	}
	fmt.Fprintf(out, "%-36s  %-24s  %-10s  %s\n", "ID", "NAME", "STATUS", "INSTANCES")
	for _, e := range list {
		fmt.Fprintf(out, "%-36s  %-24s  %-10s  %d/%d\n",
			e.ID, truncate(e.Name, 24), orDash(e.Status), e.RunningInstances, e.MaxInstances)
	}
}

// endpointURL parses `aq endpoint url <name|id>` and prints its runUrl
// verbatim — the orchestrator's own `/api/v1/run/:jobId` (still gated by an
// `x-job-token`), never a box address. Resolution reuses resolveJobID/
// findJob (resolve_job.go), the same name-or-id lookup `aq job point`/`aq
// job rm` already use, across every shape: an endpoint is still a job row.
func endpointURL(args []string) error {
	fs := flag.NewFlagSet("endpoint url", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq endpoint url <name|id>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	client := newControlClient(cred)
	id, err := resolveJobID(client, target)
	if err != nil {
		return err
	}
	ep, err := findJob(client, id)
	if err != nil {
		return err
	}
	if ep.RunURL == nil || *ep.RunURL == "" {
		return fmt.Errorf("endpoint %q has no runUrl (the server has no public base URL configured)", target)
	}
	fmt.Fprintln(os.Stdout, *ep.RunURL)
	return nil
}
