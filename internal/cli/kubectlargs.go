package cli

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/jzills/kx/internal/kubectlflags"
)

// A command that forwards to kubectl can register kubectl's own flags, read
// from the generated table in internal/kubectlflags, and let cobra parse its
// argv instead of splitting it by hand (see passthrough.go for the commands
// that still do).
//
// pflag then handles every spelling kubectl accepts — `-n x`, `-nx`,
// `--namespace=x`, `-Aw`, `--` — and finds positionals wherever they sit, so
// `kx get -n prod pods` lists pods rather than saving every row with kind
// "-n", and in `kx get pods -A 1` the 1 is an index, because the table says -A
// takes no value.
//
// Each kubectl flag's Value records the occurrence into a kubectlArgs, in the
// order pflag meets them, so what reaches kubectl is what the user gave it:
// repeats kept, values untouched. A flag kx also reads (-n, -A, -o, -w,
// --no-headers on kx get) is read back out of that argv by the same helpers as
// before, so converting a command changes how argv is split and nothing after.

// kubectlFlagAnnotation marks a flag registered from kubectl's table. Its value
// says where help lists it: kubectlFlagCommand for the subcommand's own flags,
// kubectlFlagGlobal for kubectl's global ones. A flag kx documents itself
// (shared, with kx's own usage) carries no annotation and is listed with kx's
// options.
const (
	kubectlFlagAnnotation = "kx/kubectl-flag"
	kubectlFlagCommand    = "command"
	kubectlFlagGlobal     = "global"
)

// kubectlCommandAnnotation names, on a command, the kubectl subcommand whose
// flags it registered. Help reads it to stop claiming unknown flags pass
// through: on such a command, they are refused.
const kubectlCommandAnnotation = "kx/kubectl-command"

// kubectlArgs collects the kubectl flags given to one command, in order.
type kubectlArgs struct {
	tokens []string
}

// add records one occurrence of flag with value, spelled the way kx forwards
// every flag: the long name, so a refusal quoting it reads the same as kx tree
// and kx diag already do, and the value attached with "=" so a value beginning
// with "-" cannot be read as a flag of its own. A flag given in its bare form
// is forwarded bare, which is what kubectl defaults it from.
func (a *kubectlArgs) add(flag kubectlflags.Flag, value string) {
	if flag.NoOptDefVal != "" && value == flag.NoOptDefVal {
		a.tokens = append(a.tokens, "--"+flag.Name)
		return
	}
	a.tokens = append(a.tokens, "--"+flag.Name+"="+value)
}

// kubectlValue is the pflag.Value of a kubectl flag: it records each Set rather
// than holding a typed value, since kubectl, not kx, validates it.
type kubectlValue struct {
	args  *kubectlArgs
	flag  kubectlflags.Flag
	value string
}

func (v *kubectlValue) Set(value string) error {
	v.value = value
	v.args.add(v.flag, value)
	return nil
}

func (v *kubectlValue) String() string { return v.value }

// Type is kubectl's own, so help names the value the way kubectl's does.
func (v *kubectlValue) Type() string { return v.flag.Type }

// registerKubectlFlags registers every flag kubectl defines on subcommand, and
// kubectl's global flags, on cmd, and returns the argv they record.
//
// A flag cmd already registered is shared: kx reads it as well as forwarding
// it. It keeps kx's usage text, which says what kx does with it, and takes
// kubectl's shape, so it parses exactly as kubectl would. A shorthand cmd
// already gives to a flag of its own stays kx's — kx get's -k is --key — and
// kubectl's flag is registered by its long name alone.
//
// Flag names are normalized as kubectl normalizes them, so --all_namespaces
// is --all-namespaces here as it is there.
func registerKubectlFlags(cmd *cobra.Command, subcommand string) *kubectlArgs {
	own, ok := kubectlflags.Command(subcommand)
	if !ok {
		panic("kubectlflags has no table for kubectl " + subcommand)
	}
	args := &kubectlArgs{}
	flags := cmd.Flags()
	flags.SetNormalizeFunc(wordSepNormalize)
	register := func(flag kubectlflags.Flag, where string) {
		value := &kubectlValue{args: args, flag: flag}
		if existing := flags.Lookup(flag.Name); existing != nil {
			existing.Value = value
			existing.NoOptDefVal = flag.NoOptDefVal
			existing.DefValue = ""
			return
		}
		shorthand := flag.Shorthand
		if shorthand != "" && flags.ShorthandLookup(shorthand) != nil {
			shorthand = ""
		}
		registered := flags.VarPF(value, flag.Name, shorthand, flag.Usage)
		registered.NoOptDefVal = flag.NoOptDefVal
		registered.Deprecated = flag.Deprecated
		registered.Hidden = flag.Hidden
		registered.Annotations = map[string][]string{kubectlFlagAnnotation: {where}}
	}
	for _, flag := range own {
		register(flag, kubectlFlagCommand)
	}
	for _, flag := range kubectlflags.Global() {
		// A subcommand's flag shadows a global one of the same name — kubectl
		// debug's --profile — and is already registered by the loop above.
		if flags.Lookup(flag.Name) != nil && isKubectlFlag(flags.Lookup(flag.Name)) {
			continue
		}
		register(flag, kubectlFlagGlobal)
	}

	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[kubectlCommandAnnotation] = subcommand
	cmd.SetFlagErrorFunc(unknownFlagError)
	recorders.Store(cmd, args)
	return args
}

// recorders maps each command that registered kubectl's flags to the argv
// they record, for code that sees the command but not its RunE's closure:
// machineOutput, deciding from the argv kubectl will get whether a failure may
// print a listing to stdout.
var recorders sync.Map

// recordedKubectlArgs is cmd's recorder, or nil for a command that still
// splits its argv by hand.
func recordedKubectlArgs(cmd *cobra.Command) *kubectlArgs {
	recorded, ok := recorders.Load(cmd)
	if !ok {
		return nil
	}
	return recorded.(*kubectlArgs)
}

// isKubectlFlag reports whether flag was registered from kubectl's table
// rather than by kx.
func isKubectlFlag(flag *pflag.Flag) bool { return kubectlFlagScope(flag) != "" }

// IsKubectlFlag is isKubectlFlag for the doc generators, which list kx's own
// flags in a command's signature and leave kubectl's to the placeholder.
func IsKubectlFlag(flag *pflag.Flag) bool { return isKubectlFlag(flag) }

// kubectlFlagScope is kubectlFlagCommand or kubectlFlagGlobal for a flag
// registered from kubectl's table, "" for one of kx's.
func kubectlFlagScope(flag *pflag.Flag) string {
	if scope := flag.Annotations[kubectlFlagAnnotation]; len(scope) > 0 {
		return scope[0]
	}
	return ""
}

// wordSepNormalize is kubectl's flag-name normalization (component-base's
// WordSepNormalizeFunc): an underscore is a dash.
func wordSepNormalize(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
}

// unknownFlagError replaces pflag's "unknown flag" with what kx knows about it.
//
// The flag table is read from one kubectl release, so a flag newer than it is
// unknown here though the kubectl on PATH would take it. Anything after `--`
// reaches kubectl unread, which is the way through; kx --version names both
// kubectls when the difference needs explaining.
func unknownFlagError(cmd *cobra.Command, err error) error {
	var missing *pflag.NotExistError
	if !errors.As(err, &missing) {
		return err
	}
	spelling := "--" + missing.GetSpecifiedName()
	if missing.GetSpecifiedShortnames() != "" {
		spelling = "-" + missing.GetSpecifiedName()
	}
	return fmt.Errorf(
		"The flag '%s' isn't one kx knows — its table of kubectl's flags is "+
			"from kubectl %s. To pass it to kubectl anyway, put it after --: '%s -- %s'.",
		spelling, kubectlflags.Version, requiredUse(cmd), spelling)
}

// requiredUse is cmd's path and its required arguments, the shortest
// invocation an example can be built on: "kx get <resource>".
func requiredUse(cmd *cobra.Command) string {
	parts := []string{cmd.CommandPath()}
	for _, arg := range ParseUse(cmd.Use).Args {
		if arg.Required {
			parts = append(parts, "<"+arg.Name+">")
		}
	}
	return strings.Join(parts, " ")
}

// kubectlArgv splits what cobra parsed into the positional arguments and the
// argv for kubectl: the recorded flags, then anything after `--`, unread.
func kubectlArgv(cmd *cobra.Command, args []string, recorded *kubectlArgs) (positional, forward []string) {
	positional = args
	var verbatim []string
	if at := cmd.ArgsLenAtDash(); at >= 0 {
		positional, verbatim = args[:at], args[at:]
	}
	forward = append(append([]string{}, recorded.tokens...), verbatim...)
	return positional, forward
}
