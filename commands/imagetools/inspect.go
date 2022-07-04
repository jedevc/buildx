package commands

import (
	"context"
	"fmt"

	"github.com/docker/buildx/util/driverloader"
	"github.com/docker/cli-docs-tool/annotation"
	"github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/util/appcontext"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

type inspectOptions struct {
	builder string
	format  string
	raw     bool
}

func runInspect(dockerCli command.Cli, in inspectOptions, name string) error {
	ctx := appcontext.Context()

	if in.format != "" && in.raw {
		return errors.Errorf("format and raw cannot be used together")
	}

	dis, err := driverloader.GetInstanceOrDefault(ctx, dockerCli, in.builder, "")
	if err != nil {
		return err
	}

	for _, di := range dis {
		if di.Err != nil {
			return err
		}
	}

	for _, di := range dis {
		c, err := di.Driver.Client(ctx)
		if err != nil {
			return err
		}

		_, err = c.Build(ctx, client.SolveOpt{}, "", func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
			_, _, conf, err := c.ResolveImageConfig(ctx, name, llb.ResolveImageConfigOpt{
				Type: llb.ResolveIndexType{},
			})
			if err != nil {
				return nil, err
			}
			fmt.Println(">>>", string(conf))
			return nil, nil
		}, nil)
		if err != nil {
			return err
		}
	}

	return nil

	// imageopt, err := storeutil.GetImageConfig(dockerCli, ng)
	// if err != nil {
	// 	return err
	// }

	// p, err := imagetools.NewPrinter(ctx, imageopt, name, in.format)
	// if err != nil {
	// 	return err
	// }

	// return p.Print(in.raw, dockerCli.Out())
}

func inspectCmd(dockerCli command.Cli, rootOpts RootOptions) *cobra.Command {
	var options inspectOptions

	cmd := &cobra.Command{
		Use:   "inspect [OPTIONS] NAME",
		Short: "Show details of an image in the registry",
		Args:  cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.builder = *rootOpts.Builder
			return runInspect(dockerCli, options, args[0])
		},
	}

	flags := cmd.Flags()

	flags.StringVar(&options.format, "format", "", "Format the output using the given Go template")
	flags.SetAnnotation("format", annotation.DefaultValue, []string{`"{{.Manifest}}"`})

	flags.BoolVar(&options.raw, "raw", false, "Show original, unformatted JSON manifest")

	return cmd
}
