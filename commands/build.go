package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/containerd/console"
	"github.com/docker/buildx/build"
	"github.com/docker/buildx/builder"
	"github.com/docker/buildx/controller"
	cbuild "github.com/docker/buildx/controller/build"
	"github.com/docker/buildx/controller/control"
	controllererrors "github.com/docker/buildx/controller/errdefs"
	controllerapi "github.com/docker/buildx/controller/pb"
	"github.com/docker/buildx/monitor"
	"github.com/docker/buildx/store"
	"github.com/docker/buildx/store/storeutil"
	"github.com/docker/buildx/util/buildflags"
	"github.com/docker/buildx/util/ioset"
	"github.com/docker/buildx/util/progress"
	"github.com/docker/buildx/util/tracing"
	"github.com/docker/cli-docs-tool/annotation"
	"github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	dockeropts "github.com/docker/cli/opts"
	"github.com/docker/docker/builder/remotecontext/urlutil"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/solver/errdefs"
	"github.com/moby/buildkit/util/appcontext"
	"github.com/moby/buildkit/util/grpcerrors"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/morikuni/aec"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/grpc/codes"
)

type buildOptions struct {
	allow          []string
	buildArgs      []string
	cacheFrom      []string
	cacheTo        []string
	cgroupParent   string
	contextPath    string
	contexts       []string
	dockerfileName string
	extraHosts     []string
	imageIDFile    string
	labels         []string
	networkMode    string
	noCacheFilter  []string
	outputs        []string
	platforms      []string
	printFunc      string
	secrets        []string
	shmSize        dockeropts.MemBytes
	ssh            []string
	tags           []string
	target         string
	ulimits        *dockeropts.UlimitOpt

	invoke  *invokeConfig
	noBuild bool

	attests    []string
	sbom       string
	provenance string

	progress string
	quiet    bool

	builder      string
	metadataFile string
	noCache      bool
	pull         bool
	exportPush   bool
	exportLoad   bool

	control.ControlOptions
}

func (o *buildOptions) toControllerOptions() (*controllerapi.BuildOptions, error) {
	var err error
	opts := controllerapi.BuildOptions{
		Allow:          o.allow,
		BuildArgs:      listToMap(o.buildArgs, true),
		CgroupParent:   o.cgroupParent,
		ContextPath:    o.contextPath,
		DockerfileName: o.dockerfileName,
		ExtraHosts:     o.extraHosts,
		Labels:         listToMap(o.labels, false),
		NetworkMode:    o.networkMode,
		NoCacheFilter:  o.noCacheFilter,
		Platforms:      o.platforms,
		PrintFunc:      o.printFunc,
		ShmSize:        int64(o.shmSize),
		Tags:           o.tags,
		Target:         o.target,
		Ulimits:        dockerUlimitToControllerUlimit(o.ulimits),
		Builder:        o.builder,
		MetadataFile:   o.metadataFile,
		NoCache:        o.noCache,
		Pull:           o.pull,
		ExportPush:     o.exportPush,
		ExportLoad:     o.exportLoad,
	}

	// TODO: extract env var parsing to a method easily usable by library consumers
	if v := os.Getenv("SOURCE_DATE_EPOCH"); v != "" {
		if _, ok := opts.BuildArgs["SOURCE_DATE_EPOCH"]; !ok {
			opts.BuildArgs["SOURCE_DATE_EPOCH"] = v
		}
	}

	inAttests := append([]string{}, o.attests...)
	if o.provenance != "" {
		inAttests = append(inAttests, buildflags.CanonicalizeAttest("provenance", o.provenance))
	}
	if o.sbom != "" {
		inAttests = append(inAttests, buildflags.CanonicalizeAttest("sbom", o.sbom))
	}
	opts.Attests, err = buildflags.ParseAttests(inAttests)
	if err != nil {
		return nil, err
	}

	opts.NamedContexts, err = buildflags.ParseContextNames(o.contexts)
	if err != nil {
		return nil, err
	}

	opts.Exports, err = buildflags.ParseExports(o.outputs)
	if err != nil {
		return nil, err
	}
	for _, e := range opts.Exports {
		if (e.Type == client.ExporterLocal || e.Type == client.ExporterTar) && o.imageIDFile != "" {
			return nil, errors.Errorf("local and tar exporters are incompatible with image ID file")
		}
	}

	opts.CacheFrom, err = buildflags.ParseCacheEntry(o.cacheFrom)
	if err != nil {
		return nil, err
	}
	opts.CacheTo, err = buildflags.ParseCacheEntry(o.cacheTo)
	if err != nil {
		return nil, err
	}

	opts.Secrets, err = buildflags.ParseSecretSpecs(o.secrets)
	if err != nil {
		return nil, err
	}
	opts.SSH, err = buildflags.ParseSSHSpecs(o.ssh)
	if err != nil {
		return nil, err
	}

	return &opts, nil
}

func (o *buildOptions) toProgress() (string, error) {
	switch o.progress {
	case progress.PrinterModeAuto, progress.PrinterModeTty, progress.PrinterModePlain, progress.PrinterModeQuiet:
	default:
		return "", errors.Errorf("progress=%s is not a valid progress option", o.progress)
	}

	if o.quiet {
		if o.progress != progress.PrinterModeAuto && o.progress != progress.PrinterModeQuiet {
			return "", errors.Errorf("progress=%s and quiet cannot be used together", o.progress)
		}
		return progress.PrinterModeQuiet, nil
	}
	return o.progress, nil
}

func runBuild(dockerCli command.Cli, options buildOptions) (err error) {
	ctx := appcontext.Context()
	ctx, end, err := tracing.TraceCurrentCommand(ctx, "build")
	if err != nil {
		return err
	}
	defer func() {
		end(err)
	}()

	// Avoid leaving a stale file if we eventually fail
	if options.imageIDFile != "" {
		if err := os.Remove(options.imageIDFile); err != nil && !os.IsNotExist(err) {
			return errors.Wrap(err, "removing image ID file")
		}
	}

	contextPathHash := options.contextPath
	if absContextPath, err := filepath.Abs(contextPathHash); err == nil {
		contextPathHash = absContextPath
	}
	b, err := builder.New(dockerCli,
		builder.WithName(options.builder),
		builder.WithContextPathHash(contextPathHash),
	)
	if err != nil {
		return err
	}

	ctx2, cancel := context.WithCancel(context.TODO())
	defer cancel()
	progressMode, err := options.toProgress()
	if err != nil {
		return err
	}
	var printer *progress.Printer
	printer, err = progress.NewPrinter(ctx2, os.Stderr, os.Stderr, progressMode,
		progressui.WithDesc(
			fmt.Sprintf("building with %q instance using %s driver", b.Name, b.Driver),
			fmt.Sprintf("%s:%s", b.Driver, b.Name),
		),
		progressui.WithOnDone(func(warnings []client.VertexWarning, err error) {
			printWarnings(os.Stderr, warnings, progressMode)
		}),
	)
	if err != nil {
		return err
	}

	var resp *client.SolveResponse
	var retErr error
	if isExperimental() {
		resp, retErr = runControllerBuild(ctx, dockerCli, options, printer)
	} else {
		resp, retErr = runBasicBuild(ctx, dockerCli, options, printer)
	}

	if err := printer.Wait(); retErr == nil {
		retErr = err
	}
	if retErr != nil {
		return retErr
	}

	if options.quiet {
		fmt.Println(resp.ExporterResponse[exptypes.ExporterImageDigestKey])
	}
	if options.imageIDFile != "" {
		dgst := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
		if v, ok := resp.ExporterResponse[exptypes.ExporterImageConfigDigestKey]; ok {
			dgst = v
		}
		return os.WriteFile(options.imageIDFile, []byte(dgst), 0644)
	}

	return nil
}

func runBasicBuild(ctx context.Context, dockerCli command.Cli, options buildOptions, printer *progress.Printer) (*client.SolveResponse, error) {
	opts, err := options.toControllerOptions()
	if err != nil {
		return nil, err
	}

	resp, _, err := cbuild.RunBuild(ctx, dockerCli, *opts, os.Stdin, printer, false)
	return resp, err
}

func runControllerBuild(ctx context.Context, dockerCli command.Cli, options buildOptions, printer *progress.Printer) (*client.SolveResponse, error) {
	if options.invoke != nil && (options.dockerfileName == "-" || options.contextPath == "-") {
		// stdin must be usable for monitor
		return nil, errors.Errorf("Dockerfile or context from stdin is not supported with invoke")
	}

	c, err := controller.NewController(ctx, options.ControlOptions, dockerCli, printer)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := c.Close(); err != nil {
			logrus.Warnf("failed to close server connection %v", err)
		}
	}()

	// Start build
	opts, err := options.toControllerOptions()
	if err != nil {
		return nil, err
	}

	// NOTE: buildx server has the current working directory different from the client
	// so we need to resolve paths to abosolute ones in the client.
	opts, err = resolvePaths(opts)
	if err != nil {
		return nil, err
	}

	var ref string
	var retErr error
	var resp *client.SolveResponse
	f := ioset.NewSingleForwarder()
	f.SetReader(os.Stdin)
	if !options.noBuild {
		pr, pw := io.Pipe()
		f.SetWriter(pw, func() io.WriteCloser {
			pw.Close() // propagate EOF
			logrus.Debug("propagating stdin close")
			return nil
		})

		ref, resp, err = c.Build(ctx, *opts, pr, printer)
		if err != nil {
			var be *controllererrors.BuildError
			if errors.As(err, &be) {
				ref = be.Ref
				retErr = err
				// We can proceed to monitor
			} else {
				return nil, errors.Wrapf(err, "failed to build")
			}
		}

		if err := pw.Close(); err != nil {
			logrus.Debug("failed to close stdin pipe writer")
		}
		if err := pr.Close(); err != nil {
			logrus.Debug("failed to close stdin pipe reader")
		}
	}

	// post-build operations
	if options.invoke != nil && options.invoke.needsMonitor(retErr) {
		pr2, pw2 := io.Pipe()
		f.SetWriter(pw2, func() io.WriteCloser {
			pw2.Close() // propagate EOF
			return nil
		})
		con := console.Current()
		if err := con.SetRaw(); err != nil {
			if err := c.Disconnect(ctx, ref); err != nil {
				logrus.Warnf("disconnect error: %v", err)
			}
			return nil, errors.Errorf("failed to configure terminal: %v", err)
		}
		err = monitor.RunMonitor(ctx, ref, opts, options.invoke.InvokeConfig, c, pr2, os.Stdout, os.Stderr, printer)
		con.Reset()
		if err := pw2.Close(); err != nil {
			logrus.Debug("failed to close monitor stdin pipe reader")
		}
		if err != nil {
			logrus.Warnf("failed to run monitor: %v", err)
		}
	} else {
		if err := c.Disconnect(ctx, ref); err != nil {
			logrus.Warnf("disconnect error: %v", err)
		}
	}

	return resp, retErr
}

func buildCmd(dockerCli command.Cli, rootOpts *rootOptions) *cobra.Command {
	options := buildOptions{}
	cFlags := &commonFlags{}
	var invokeFlag string

	cmd := &cobra.Command{
		Use:     "build [OPTIONS] PATH | URL | -",
		Aliases: []string{"b"},
		Short:   "Start a build",
		Args:    cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.contextPath = args[0]
			options.builder = rootOpts.builder
			options.metadataFile = cFlags.metadataFile
			options.noCache = false
			if cFlags.noCache != nil {
				options.noCache = *cFlags.noCache
			}
			options.pull = false
			if cFlags.pull != nil {
				options.pull = *cFlags.pull
			}
			options.progress = cFlags.progress
			cmd.Flags().VisitAll(checkWarnedFlags)

			if invokeFlag != "" {
				invoke, err := parseInvokeConfig(invokeFlag)
				if err != nil {
					return err
				}
				options.invoke = &invoke
				options.noBuild = invokeFlag == "debug-shell"
			}
			return runBuild(dockerCli, options)
		},
	}

	var platformsDefault []string
	if v := os.Getenv("DOCKER_DEFAULT_PLATFORM"); v != "" {
		platformsDefault = []string{v}
	}

	flags := cmd.Flags()

	flags.StringSliceVar(&options.extraHosts, "add-host", []string{}, `Add a custom host-to-IP mapping (format: "host:ip")`)
	flags.SetAnnotation("add-host", annotation.ExternalURL, []string{"https://docs.docker.com/engine/reference/commandline/build/#add-host"})

	flags.StringSliceVar(&options.allow, "allow", []string{}, `Allow extra privileged entitlement (e.g., "network.host", "security.insecure")`)

	flags.StringArrayVar(&options.buildArgs, "build-arg", []string{}, "Set build-time variables")

	flags.StringArrayVar(&options.cacheFrom, "cache-from", []string{}, `External cache sources (e.g., "user/app:cache", "type=local,src=path/to/dir")`)

	flags.StringArrayVar(&options.cacheTo, "cache-to", []string{}, `Cache export destinations (e.g., "user/app:cache", "type=local,dest=path/to/dir")`)

	flags.StringVar(&options.cgroupParent, "cgroup-parent", "", "Optional parent cgroup for the container")
	flags.SetAnnotation("cgroup-parent", annotation.ExternalURL, []string{"https://docs.docker.com/engine/reference/commandline/build/#cgroup-parent"})

	flags.StringArrayVar(&options.contexts, "build-context", []string{}, "Additional build contexts (e.g., name=path)")

	flags.StringVarP(&options.dockerfileName, "file", "f", "", `Name of the Dockerfile (default: "PATH/Dockerfile")`)
	flags.SetAnnotation("file", annotation.ExternalURL, []string{"https://docs.docker.com/engine/reference/commandline/build/#file"})

	flags.StringVar(&options.imageIDFile, "iidfile", "", "Write the image ID to the file")

	flags.StringArrayVar(&options.labels, "label", []string{}, "Set metadata for an image")

	flags.BoolVar(&options.exportLoad, "load", false, `Shorthand for "--output=type=docker"`)

	flags.StringVar(&options.networkMode, "network", "default", `Set the networking mode for the "RUN" instructions during build`)

	flags.StringArrayVar(&options.noCacheFilter, "no-cache-filter", []string{}, "Do not cache specified stages")

	flags.StringArrayVarP(&options.outputs, "output", "o", []string{}, `Output destination (format: "type=local,dest=path")`)

	flags.StringArrayVar(&options.platforms, "platform", platformsDefault, "Set target platform for build")

	if isExperimental() {
		flags.StringVar(&options.printFunc, "print", "", "Print result of information request (e.g., outline, targets) [experimental]")
	}

	flags.BoolVar(&options.exportPush, "push", false, `Shorthand for "--output=type=registry"`)

	flags.BoolVarP(&options.quiet, "quiet", "q", false, "Suppress the build output and print image ID on success")

	flags.StringArrayVar(&options.secrets, "secret", []string{}, `Secret to expose to the build (format: "id=mysecret[,src=/local/secret]")`)

	flags.Var(&options.shmSize, "shm-size", `Size of "/dev/shm"`)

	flags.StringArrayVar(&options.ssh, "ssh", []string{}, `SSH agent socket or keys to expose to the build (format: "default|<id>[=<socket>|<key>[,<key>]]")`)

	flags.StringArrayVarP(&options.tags, "tag", "t", []string{}, `Name and optionally a tag (format: "name:tag")`)
	flags.SetAnnotation("tag", annotation.ExternalURL, []string{"https://docs.docker.com/engine/reference/commandline/build/#tag"})

	flags.StringVar(&options.target, "target", "", "Set the target build stage to build")
	flags.SetAnnotation("target", annotation.ExternalURL, []string{"https://docs.docker.com/engine/reference/commandline/build/#target"})

	options.ulimits = dockeropts.NewUlimitOpt(nil)
	flags.Var(options.ulimits, "ulimit", "Ulimit options")

	flags.StringArrayVar(&options.attests, "attest", []string{}, `Attestation parameters (format: "type=sbom,generator=image")`)
	flags.StringVar(&options.sbom, "sbom", "", `Shorthand for "--attest=type=sbom"`)
	flags.StringVar(&options.provenance, "provenance", "", `Shortand for "--attest=type=provenance"`)

	if isExperimental() {
		flags.StringVar(&invokeFlag, "invoke", "", "Invoke a command after the build [experimental]")
		flags.StringVar(&options.Root, "root", "", "Specify root directory of server to connect [experimental]")
		flags.BoolVar(&options.Detach, "detach", runtime.GOOS == "linux", "Detach buildx server (supported only on linux) [experimental]")
		flags.StringVar(&options.ServerConfig, "server-config", "", "Specify buildx server config file (used only when launching new server) [experimental]")
	}

	// hidden flags
	var ignore string
	var ignoreSlice []string
	var ignoreBool bool
	var ignoreInt int64

	flags.BoolVar(&ignoreBool, "compress", false, "Compress the build context using gzip")
	flags.MarkHidden("compress")

	flags.StringVar(&ignore, "isolation", "", "Container isolation technology")
	flags.MarkHidden("isolation")
	flags.SetAnnotation("isolation", "flag-warn", []string{"isolation flag is deprecated with BuildKit."})

	flags.StringSliceVar(&ignoreSlice, "security-opt", []string{}, "Security options")
	flags.MarkHidden("security-opt")
	flags.SetAnnotation("security-opt", "flag-warn", []string{`security-opt flag is deprecated. "RUN --security=insecure" should be used with BuildKit.`})

	flags.BoolVar(&ignoreBool, "squash", false, "Squash newly built layers into a single new layer")
	flags.MarkHidden("squash")
	flags.SetAnnotation("squash", "flag-warn", []string{"experimental flag squash is removed with BuildKit. You should squash inside build using a multi-stage Dockerfile for efficiency."})

	flags.StringVarP(&ignore, "memory", "m", "", "Memory limit")
	flags.MarkHidden("memory")

	flags.StringVar(&ignore, "memory-swap", "", `Swap limit equal to memory plus swap: "-1" to enable unlimited swap`)
	flags.MarkHidden("memory-swap")

	flags.Int64VarP(&ignoreInt, "cpu-shares", "c", 0, "CPU shares (relative weight)")
	flags.MarkHidden("cpu-shares")

	flags.Int64Var(&ignoreInt, "cpu-period", 0, "Limit the CPU CFS (Completely Fair Scheduler) period")
	flags.MarkHidden("cpu-period")

	flags.Int64Var(&ignoreInt, "cpu-quota", 0, "Limit the CPU CFS (Completely Fair Scheduler) quota")
	flags.MarkHidden("cpu-quota")

	flags.StringVar(&ignore, "cpuset-cpus", "", `CPUs in which to allow execution ("0-3", "0,1")`)
	flags.MarkHidden("cpuset-cpus")

	flags.StringVar(&ignore, "cpuset-mems", "", `MEMs in which to allow execution ("0-3", "0,1")`)
	flags.MarkHidden("cpuset-mems")

	flags.BoolVar(&ignoreBool, "rm", true, "Remove intermediate containers after a successful build")
	flags.MarkHidden("rm")

	flags.BoolVar(&ignoreBool, "force-rm", false, "Always remove intermediate containers")
	flags.MarkHidden("force-rm")

	commonBuildFlags(cFlags, flags)
	return cmd
}

// comomnFlags is a set of flags commonly shared among subcommands.
type commonFlags struct {
	metadataFile string
	progress     string
	noCache      *bool
	pull         *bool
}

func commonBuildFlags(options *commonFlags, flags *pflag.FlagSet) {
	options.noCache = flags.Bool("no-cache", false, "Do not use cache when building the image")
	flags.StringVar(&options.progress, "progress", "auto", `Set type of progress output ("auto", "plain", "tty"). Use plain to show container output`)
	options.pull = flags.Bool("pull", false, "Always attempt to pull all referenced images")
	flags.StringVar(&options.metadataFile, "metadata-file", "", "Write build result metadata to the file")
}

func checkWarnedFlags(f *pflag.Flag) {
	if !f.Changed {
		return
	}
	for t, m := range f.Annotations {
		switch t {
		case "flag-warn":
			logrus.Warn(m[0])
		}
	}
}

func writeMetadataFile(filename string, dt interface{}) error {
	b, err := json.MarshalIndent(dt, "", "  ")
	if err != nil {
		return err
	}
	return ioutils.AtomicWriteFile(filename, b, 0644)
}

func decodeExporterResponse(exporterResponse map[string]string) map[string]interface{} {
	out := make(map[string]interface{})
	for k, v := range exporterResponse {
		dt, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			out[k] = v
			continue
		}
		var raw map[string]interface{}
		if err = json.Unmarshal(dt, &raw); err != nil || len(raw) == 0 {
			out[k] = v
			continue
		}
		out[k] = json.RawMessage(dt)
	}
	return out
}

func wrapBuildError(err error, bake bool) error {
	if err == nil {
		return nil
	}
	st, ok := grpcerrors.AsGRPCStatus(err)
	if ok {
		if st.Code() == codes.Unimplemented && strings.Contains(st.Message(), "unsupported frontend capability moby.buildkit.frontend.contexts") {
			msg := "current frontend does not support --build-context."
			if bake {
				msg = "current frontend does not support defining additional contexts for targets."
			}
			msg += " Named contexts are supported since Dockerfile v1.4. Use #syntax directive in Dockerfile or update to latest BuildKit."
			return &wrapped{err, msg}
		}
	}
	return err
}

type wrapped struct {
	err error
	msg string
}

func (w *wrapped) Error() string {
	return w.msg
}

func (w *wrapped) Unwrap() error {
	return w.err
}

func isExperimental() bool {
	if v, ok := os.LookupEnv("BUILDX_EXPERIMENTAL"); ok {
		vv, _ := strconv.ParseBool(v)
		return vv
	}
	return false
}

func updateLastActivity(dockerCli command.Cli, ng *store.NodeGroup) error {
	txn, release, err := storeutil.GetStore(dockerCli)
	if err != nil {
		return err
	}
	defer release()
	return txn.UpdateLastActivity(ng)
}

type invokeConfig struct {
	controllerapi.InvokeConfig
	invokeFlag string
}

func (cfg *invokeConfig) needsMonitor(retErr error) bool {
	switch cfg.invokeFlag {
	case "debug-shell":
		return true
	case "on-error":
		return retErr != nil
	default:
		return cfg.invokeFlag != ""
	}
}

func parseInvokeConfig(invoke string) (cfg invokeConfig, err error) {
	cfg.invokeFlag = invoke
	cfg.Tty = true
	switch invoke {
	case "default", "debug-shell":
		return cfg, nil
	case "on-error":
		// NOTE: we overwrite the command to run because the original one should fail on the failed step.
		// TODO: make this configurable via flags or restorable from LLB.
		// Discussion: https://github.com/docker/buildx/pull/1640#discussion_r1113295900
		cfg.Cmd = []string{"/bin/sh"}
		return cfg, nil
	}

	csvReader := csv.NewReader(strings.NewReader(invoke))
	fields, err := csvReader.Read()
	if err != nil {
		return cfg, err
	}
	if len(fields) == 1 && !strings.Contains(fields[0], "=") {
		cfg.Cmd = []string{fields[0]}
		return cfg, nil
	}
	cfg.NoUser = true
	cfg.NoCwd = true
	for _, field := range fields {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 {
			return cfg, errors.Errorf("invalid value %s", field)
		}
		key := strings.ToLower(parts[0])
		value := parts[1]
		switch key {
		case "args":
			cfg.Cmd = append(cfg.Cmd, value) // TODO: support JSON
		case "entrypoint":
			cfg.Entrypoint = append(cfg.Entrypoint, value) // TODO: support JSON
		case "env":
			cfg.Env = append(cfg.Env, value)
		case "user":
			cfg.User = value
			cfg.NoUser = false
		case "cwd":
			cfg.Cwd = value
			cfg.NoCwd = false
		case "tty":
			cfg.Tty, err = strconv.ParseBool(value)
			if err != nil {
				return cfg, errors.Errorf("failed to parse tty: %v", err)
			}
		default:
			return cfg, errors.Errorf("unknown key %q", key)
		}
	}
	return cfg, nil
}

func listToMap(values []string, defaultEnv bool) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		kv := strings.SplitN(value, "=", 2)
		if len(kv) == 1 {
			if defaultEnv {
				v, ok := os.LookupEnv(kv[0])
				if ok {
					result[kv[0]] = v
				}
			} else {
				result[kv[0]] = ""
			}
		} else {
			result[kv[0]] = kv[1]
		}
	}
	return result
}

func dockerUlimitToControllerUlimit(u *dockeropts.UlimitOpt) *controllerapi.UlimitOpt {
	if u == nil {
		return nil
	}
	values := make(map[string]*controllerapi.Ulimit)
	for _, u := range u.GetList() {
		values[u.Name] = &controllerapi.Ulimit{
			Name: u.Name,
			Hard: u.Hard,
			Soft: u.Soft,
		}
	}
	return &controllerapi.UlimitOpt{Values: values}
}

// resolvePaths resolves all paths contained in controllerapi.BuildOptions
// and replaces them to absolute paths.
func resolvePaths(options *controllerapi.BuildOptions) (_ *controllerapi.BuildOptions, err error) {
	localContext := false
	if options.ContextPath != "" && options.ContextPath != "-" {
		if !build.IsRemoteURL(options.ContextPath) {
			localContext = true
			options.ContextPath, err = filepath.Abs(options.ContextPath)
			if err != nil {
				return nil, err
			}
		}
	}
	if options.DockerfileName != "" && options.DockerfileName != "-" {
		if localContext && !urlutil.IsURL(options.DockerfileName) {
			options.DockerfileName, err = filepath.Abs(options.DockerfileName)
			if err != nil {
				return nil, err
			}
		}
	}

	var contexts map[string]string
	for k, v := range options.NamedContexts {
		if build.IsRemoteURL(v) || strings.HasPrefix(v, "docker-image://") {
			// url prefix, this is a remote path
		} else if strings.HasPrefix(v, "oci-layout://") {
			// oci layout prefix, this is a local path
			p := strings.TrimPrefix(v, "oci-layout://")
			p, err = filepath.Abs(p)
			if err != nil {
				return nil, err
			}
			v = "oci-layout://" + p
		} else {
			// no prefix, assume local path
			v, err = filepath.Abs(v)
			if err != nil {
				return nil, err
			}
		}

		if contexts == nil {
			contexts = make(map[string]string)
		}
		contexts[k] = v
	}
	options.NamedContexts = contexts

	var cacheFrom []*controllerapi.CacheOptionsEntry
	for _, co := range options.CacheFrom {
		switch co.Type {
		case "local":
			var attrs map[string]string
			for k, v := range co.Attrs {
				if attrs == nil {
					attrs = make(map[string]string)
				}
				switch k {
				case "src":
					p := v
					if p != "" {
						p, err = filepath.Abs(p)
						if err != nil {
							return nil, err
						}
					}
					attrs[k] = p
				default:
					attrs[k] = v
				}
			}
			co.Attrs = attrs
			cacheFrom = append(cacheFrom, co)
		default:
			cacheFrom = append(cacheFrom, co)
		}
	}
	options.CacheFrom = cacheFrom

	var cacheTo []*controllerapi.CacheOptionsEntry
	for _, co := range options.CacheTo {
		switch co.Type {
		case "local":
			var attrs map[string]string
			for k, v := range co.Attrs {
				if attrs == nil {
					attrs = make(map[string]string)
				}
				switch k {
				case "dest":
					p := v
					if p != "" {
						p, err = filepath.Abs(p)
						if err != nil {
							return nil, err
						}
					}
					attrs[k] = p
				default:
					attrs[k] = v
				}
			}
			co.Attrs = attrs
			cacheTo = append(cacheTo, co)
		default:
			cacheTo = append(cacheTo, co)
		}
	}
	options.CacheTo = cacheTo
	var exports []*controllerapi.ExportEntry
	for _, e := range options.Exports {
		if e.Destination != "" && e.Destination != "-" {
			e.Destination, err = filepath.Abs(e.Destination)
			if err != nil {
				return nil, err
			}
		}
		exports = append(exports, e)
	}
	options.Exports = exports

	var secrets []*controllerapi.Secret
	for _, s := range options.Secrets {
		if s.FilePath != "" {
			s.FilePath, err = filepath.Abs(s.FilePath)
			if err != nil {
				return nil, err
			}
		}
		secrets = append(secrets, s)
	}
	options.Secrets = secrets

	var ssh []*controllerapi.SSH
	for _, s := range options.SSH {
		var ps []string
		for _, pt := range s.Paths {
			p := pt
			if p != "" {
				p, err = filepath.Abs(p)
				if err != nil {
					return nil, err
				}
			}
			ps = append(ps, p)

		}
		s.Paths = ps
		ssh = append(ssh, s)
	}
	options.SSH = ssh

	if options.MetadataFile != "" {
		options.MetadataFile, err = filepath.Abs(options.MetadataFile)
		if err != nil {
			return nil, err
		}
	}

	return options, nil
}

func printWarnings(w io.Writer, warnings []client.VertexWarning, mode string) {
	if len(warnings) == 0 || mode == progress.PrinterModeQuiet {
		return
	}
	fmt.Fprintf(w, "\n ")
	sb := &bytes.Buffer{}
	if len(warnings) == 1 {
		fmt.Fprintf(sb, "1 warning found")
	} else {
		fmt.Fprintf(sb, "%d warnings found", len(warnings))
	}
	if logrus.GetLevel() < logrus.DebugLevel {
		fmt.Fprintf(sb, " (use --debug to expand)")
	}
	fmt.Fprintf(sb, ":\n")
	fmt.Fprint(w, aec.Apply(sb.String(), aec.YellowF))

	for _, warn := range warnings {
		fmt.Fprintf(w, " - %s\n", warn.Short)
		if logrus.GetLevel() < logrus.DebugLevel {
			continue
		}
		for _, d := range warn.Detail {
			fmt.Fprintf(w, "%s\n", d)
		}
		if warn.URL != "" {
			fmt.Fprintf(w, "More info: %s\n", warn.URL)
		}
		if warn.SourceInfo != nil && warn.Range != nil {
			src := errdefs.Source{
				Info:   warn.SourceInfo,
				Ranges: warn.Range,
			}
			src.Print(w)
		}
		fmt.Fprintf(w, "\n")

	}
}
