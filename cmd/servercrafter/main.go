// Command servercrafter builds Minecraft server directories from declarative templates.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/discowzombie/servercrafter/internal/bootstrap"
	"github.com/discowzombie/servercrafter/internal/build"
	"github.com/discowzombie/servercrafter/internal/fetch"
	"github.com/discowzombie/servercrafter/internal/project"
	"github.com/discowzombie/servercrafter/internal/resolve"
	"github.com/discowzombie/servercrafter/internal/template"
	"github.com/getsentry/sentry-go"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
	"go.yaml.in/yaml/v3"
)

// version is set at build time with -ldflags "-X main.version=…"
var version = "dev"

var log = logrus.New()

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sentryEnabled := false
	root := &cli.Command{
		Name:    "servercrafter",
		Usage:   "build Minecraft server directories from declarative templates",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "project",
				Aliases: []string{"p"},
				Usage:   "project file declaring the templates directory, sources and credentials",
				Value:   project.DefaultFile,
				Sources: cli.EnvVars("SERVERCRAFTER_PROJECT"),
			},
			&cli.StringFlag{
				Name:    "log-level",
				Usage:   "trace, debug, info, warn or error",
				Value:   "info",
				Sources: cli.EnvVars("SERVERCRAFTER_LOG_LEVEL"),
			},
			&cli.StringFlag{
				Name:    "log-format",
				Usage:   "text or json",
				Value:   "text",
				Sources: cli.EnvVars("SERVERCRAFTER_LOG_FORMAT"),
			},
			&cli.StringFlag{
				Name:    "sentry-dsn",
				Usage:   "report errors to Sentry",
				Sources: cli.EnvVars("SENTRY_DSN"),
			},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			level, err := logrus.ParseLevel(cmd.String("log-level"))
			if err != nil {
				return ctx, fmt.Errorf("--log-level: %w", err)
			}
			log.SetLevel(level)
			log.SetOutput(os.Stderr)
			switch cmd.String("log-format") {
			case "json":
				log.SetFormatter(&logrus.JSONFormatter{})
			case "text":
				log.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
			default:
				return ctx, errors.New("--log-format: text or json")
			}
			if dsn := cmd.String("sentry-dsn"); dsn != "" {
				if err = sentry.Init(sentry.ClientOptions{Dsn: dsn, Release: "servercrafter@" + version}); err != nil {
					return ctx, fmt.Errorf("sentry: %w", err)
				}
				sentryEnabled = true
			}
			return ctx, nil
		},
		Commands: []*cli.Command{
			buildCommand(),
			validateCommand(),
			listCommand(),
			renderCommand(),
			diffCommand(),
			bootstrapCommand(),
		},
	}
	err := root.Run(ctx, os.Args)
	if err != nil {
		if sentryEnabled {
			sentry.CaptureException(err)
			sentry.Flush(2 * time.Second)
		}
		var exit cli.ExitCoder
		if errors.As(err, &exit) {
			if msg := exit.Error(); msg != "" {
				log.Error(msg)
			}
			os.Exit(exit.ExitCode())
		}
		for _, line := range strings.Split(err.Error(), "\n") {
			log.Error(line)
		}
		os.Exit(1)
	}
}

func buildCommand() *cli.Command {
	return &cli.Command{
		Name:      "build",
		Usage:     "build a template into a server directory",
		ArgsUsage: "<template id or file>",
		Flags: append([]cli.Flag{
			&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "server directory", Value: "server"},
			&cli.BoolFlag{Name: "clean", Usage: "empty the output directory first (only when servercrafter built it)"},
			&cli.BoolFlag{Name: "refresh", Usage: "download every artifact again, even those already up to date"},
		}, resolveFlags()...),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			env, err := loadTemplate(cmd)
			if err != nil {
				return err
			}
			defer env.resolver.Close()
			start := time.Now()
			m, err := build.Run(ctx, build.Options{
				Project:   env.project,
				Template:  env.template,
				Resolver:  env.resolver,
				OutDir:    cmd.String("output"),
				Overrides: env.overrides,
				Jobs:      cmd.Int("jobs"),
				Clean:     cmd.Bool("clean"),
				Refresh:   cmd.Bool("refresh"),
				Env:       os.LookupEnv,
				Log:       log,
				Tool:      version,
			})
			if err != nil {
				return err
			}
			log.Infof("Built %s into %s: %d artifacts in %s", m.Template, cmd.String("output"),
				len(m.Entries), time.Since(start).Round(time.Millisecond))
			return nil
		},
	}
}

func diffCommand() *cli.Command {
	return &cli.Command{
		Name:      "diff",
		Usage:     "show what a build would change in a server directory, without downloading anything",
		ArgsUsage: "<template id or file>",
		Flags: append([]cli.Flag{
			&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "server directory", Value: "server"},
			&cli.BoolFlag{Name: "exit-code", Usage: "exit with status 2 when the server is not up to date"},
			&cli.BoolFlag{Name: "all", Usage: "also list unchanged artifacts"},
		}, resolveFlags()...),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			env, err := loadTemplate(cmd)
			if err != nil {
				return err
			}
			defer env.resolver.Close()
			changes, err := build.Diff(ctx, build.Options{
				Project:   env.project,
				Template:  env.template,
				Resolver:  env.resolver,
				OutDir:    cmd.String("output"),
				Overrides: env.overrides,
				Jobs:      cmd.Int("jobs"),
				Env:       os.LookupEnv,
				Log:       log,
			})
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			pending := 0
			for _, c := range changes {
				if c.Status != build.StatusUnchanged {
					pending++
				} else if !cmd.Bool("all") {
					continue
				}
				versions := c.To
				if c.From != c.To && c.From != "" {
					versions = c.From + " -> " + c.To
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Status, c.ID, c.Path, versions)
			}
			_ = w.Flush()
			if pending == 0 {
				fmt.Println("Up to date")
			}
			if pending > 0 && cmd.Bool("exit-code") {
				return cli.Exit("", 2)
			}
			return nil
		},
	}
}

func resolveFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringSliceFlag{Name: "set", Usage: "override the version of an artifact: server=1.21.4, <jarId>=1.2.0, world:<name>=… (repeatable)"},
		&cli.IntFlag{Name: "jobs", Aliases: []string{"j"}, Usage: "parallel downloads", Value: 4},
	}
}

func validateCommand() *cli.Command {
	return &cli.Command{
		Name:      "validate",
		Usage:     "check the project and its templates without downloading anything",
		ArgsUsage: "[template id or file…]",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "resolve", Usage: "also resolve every artifact against its source (network access)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			p, loader, err := loadProject(cmd)
			if err != nil {
				return err
			}
			// Every file is parsed, partials included, so that a broken partial is reported even
			// when no buildable template uses it
			if _, err = loader.All(); err != nil {
				return err
			}
			refs := cmd.Args().Slice()
			if len(refs) == 0 {
				buildable, err := loader.Buildable()
				if err != nil {
					return err
				}
				for _, t := range buildable {
					refs = append(refs, t.ID)
				}
			}
			resolver := newResolver(p)
			defer resolver.Close()
			var errs []error
			for _, ref := range refs {
				if err := validateTemplate(ctx, cmd, p, loader, resolver, ref); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", ref, err))
					continue
				}
				log.Infof("%s is valid", ref)
			}
			if len(errs) > 0 {
				return errors.Join(errs...)
			}
			log.Infof("%d templates valid", len(refs))
			return nil
		},
	}
}

func validateTemplate(ctx context.Context, cmd *cli.Command, p *project.Project, loader *template.Loader,
	resolver *resolve.Resolver, ref string) error {
	t, err := loader.Find(ref)
	if err != nil {
		return err
	}
	flat, err := loader.Flatten(t)
	if err != nil {
		return err
	}
	if err = flat.ValidateBuildable(); err != nil {
		return err
	}
	if err = flat.CheckProject(p); err != nil {
		return err
	}
	items, err := build.Plan(flat, nil, os.LookupEnv, logrus.NewEntry(log).WithField("template", ref))
	if err != nil {
		return err
	}
	if !cmd.Bool("resolve") {
		return nil
	}
	var errs []error
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, item := range items {
		res, err := resolver.Resolve(ctx, item.Artifact)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", item.ID, err))
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.ID, item.ID, res.Version, res.Origin)
	}
	_ = w.Flush()
	return errors.Join(errs...)
}

func listCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list the buildable templates",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			_, loader, err := loadProject(cmd)
			if err != nil {
				return err
			}
			templates, err := loader.Buildable()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			for _, t := range templates {
				chain, err := loader.Chain(t)
				if err != nil {
					return err
				}
				fmt.Fprintf(w, "%s\t%s\n", t.ID, strings.Join(chain, " < "))
			}
			return w.Flush()
		},
	}
}

func renderCommand() *cli.Command {
	return &cli.Command{
		Name:      "render",
		Usage:     "print a template with its parents merged, to debug inheritance",
		ArgsUsage: "<template id or file>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			_, loader, err := loadProject(cmd)
			if err != nil {
				return err
			}
			if cmd.Args().Len() != 1 {
				return errors.New("expected one template")
			}
			t, err := loader.Find(cmd.Args().First())
			if err != nil {
				return err
			}
			flat, err := loader.Flatten(t)
			if err != nil {
				return err
			}
			chain, err := loader.Chain(t)
			if err != nil {
				return err
			}
			fmt.Printf("# %s, merged from %s\n", t.ID, strings.Join(chain, ", "))
			encoder := yaml.NewEncoder(os.Stdout)
			encoder.SetIndent(2)
			if err = encoder.Encode(flat); err != nil {
				return err
			}
			return encoder.Close()
		},
	}
}

func bootstrapCommand() *cli.Command {
	return &cli.Command{
		Name:      "bootstrap",
		Usage:     "verify a built server, apply its runtime patches, then exec the server (container entrypoint)",
		ArgsUsage: "[-- command args…]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "target", Aliases: []string{"t"}, Usage: "server directory", Value: "."},
			&cli.StringFlag{Name: "manifest", Aliases: []string{"m"}, Usage: "manifest to read (default: <target>/servercrafter-manifest.json)"},
			&cli.BoolFlag{Name: "no-verify", Usage: "skip the checksum verification of the installed files"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			err := bootstrap.Prepare(bootstrap.Options{
				Target:   cmd.String("target"),
				Manifest: cmd.String("manifest"),
				Verify:   !cmd.Bool("no-verify"),
				Env:      os.LookupEnv,
				Log:      log,
			})
			if err != nil {
				return err
			}
			command := cmd.Args().Slice()
			if len(command) == 0 {
				return nil
			}
			log.Infof("Starting %s", strings.Join(command, " "))
			return bootstrap.Exec("", command)
		},
	}
}

type templateEnv struct {
	project   *project.Project
	template  *template.Template
	resolver  *resolve.Resolver
	overrides map[string]string
}

func loadTemplate(cmd *cli.Command) (*templateEnv, error) {
	p, loader, err := loadProject(cmd)
	if err != nil {
		return nil, err
	}
	if cmd.Args().Len() != 1 {
		return nil, errors.New("expected one template: its id, or the path to its file")
	}
	t, err := loader.Find(cmd.Args().First())
	if err != nil {
		return nil, err
	}
	flat, err := loader.Flatten(t)
	if err != nil {
		return nil, err
	}
	overrides := map[string]string{}
	for _, set := range cmd.StringSlice("set") {
		key, value, ok := strings.Cut(set, "=")
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("--set %q: expected <artifact>=<version>", set)
		}
		overrides[key] = value
	}
	return &templateEnv{project: p, template: flat, resolver: newResolver(p), overrides: overrides}, nil
}

func loadProject(cmd *cli.Command) (*project.Project, *template.Loader, error) {
	p, err := project.LoadOrImplicit(cmd.String("project"), cmd.IsSet("project"))
	if err != nil {
		return nil, nil, err
	}
	if p.Path != "" {
		log.Debugf("Using project %s", p.Path)
	}
	return p, template.NewLoader(p.TemplatesPath()), nil
}

func newResolver(p *project.Project) *resolve.Resolver {
	client := fetch.NewClient("servercrafter/"+version+" (+https://github.com/discowzombie/servercrafter)", log)
	return resolve.New(p, client, log)
}
