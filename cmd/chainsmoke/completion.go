package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/urfave/cli/v2"
)

// -----------------------------------------------------------------------------
// Shell completion (TASKS.md T8.3)
// -----------------------------------------------------------------------------
//
// urfave/cli already answers the completion PROTOCOL: with EnableBashCompletion
// set, `chainsmoke <words> --generate-bash-completion` prints the candidates for
// that position. What it does not ship is anything a user can install, only two
// files in its own repo — so `chainsmoke completion bash` emits the glue here.
//
// Fish is different and gets the library's real generator: fish completions are
// declarative, so ToFishCompletion writes out every verb, alias and flag with
// its usage text, which is strictly better than a callback.

// bannerCorruptsOutput reports whether printing the wordmark would break what
// the user actually asked for.
//
// Two cases, and both are real rather than theoretical. During the completion
// PROTOCOL every banner line becomes a completion candidate, so pressing TAB
// would offer "________" and "======" alongside the verbs. And
// `chainsmoke completion bash >> ~/.bashrc` would paste ASCII art into a file
// the shell then tries to execute.
//
// This is NOT the general banner fix (T10.1/T10.2 owns moving it behind a TTY
// check); it is the narrow suppression that makes T8.3 possible at all.
func bannerCorruptsOutput(args []string) bool {
	if len(args) < 2 {
		return false
	}
	// Same test urfave itself makes: the shell function always appends the flag
	// last, and a "--" anywhere disables completion.
	if args[len(args)-1] == "--generate-bash-completion" && !slices.Contains(args, "--") {
		return true
	}
	return firstVerb(args[1:]) == "completion"
}

// valueFlags are the global flags that consume the NEXT argument. Kept as an
// explicit list because firstVerb has to skip those values, and getting it
// wrong would read `--proxy completion` as the completion verb. It must stay in
// step with the global flag set in main().
var valueFlags = []string{"proxy", "impersonate", "tor-socks"}

// firstVerb returns the first argument that is a command name rather than a
// global flag or a flag's value. "" when there is none.
func firstVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return ""
		}
		if !strings.HasPrefix(a, "-") {
			return a
		}
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") { // --proxy=... carries its own value
			continue
		}
		if slices.Contains(valueFlags, name) {
			i++ // skip the value
		}
	}
	return ""
}

func completionCommand() *cli.Command {
	return &cli.Command{
		Name:      "completion",
		Usage:     "Print a shell completion script",
		ArgsUsage: "<bash|zsh|fish>",
		Description: "Install it by sourcing the output:\n" +
			"      chainsmoke completion bash >> ~/.bashrc\n" +
			"      chainsmoke completion zsh  > \"${fpath[1]}/_chainsmoke\"\n" +
			"      chainsmoke completion fish > ~/.config/fish/completions/chainsmoke.fish\n" +
			"   The bash and zsh scripts ask the binary what fits at the cursor, so they\n" +
			"   stay correct as verbs are added. No banner is printed on this path.",
		Action: func(c *cli.Context) error {
			switch c.Args().First() {
			case "bash":
				fmt.Print(bashCompletion)
			case "zsh":
				fmt.Print(zshCompletion)
			case "fish":
				// The library's generator: declarative, with usage strings.
				s, err := c.App.ToFishCompletion()
				if err != nil {
					return fmt.Errorf("generate fish completion: %w", err)
				}
				fmt.Println(s)
			case "":
				return fmt.Errorf("usage: chainsmoke completion <bash|zsh|fish>")
			default:
				return fmt.Errorf("unknown shell %q — supported: bash, zsh, fish", c.Args().First())
			}
			return nil
		},
		BashComplete: func(c *cli.Context) {
			for _, s := range []string{"bash", "zsh", "fish"} {
				fmt.Fprintln(os.Stdout, s)
			}
		},
	}
}

// bashCompletion re-asks the binary at every keystroke rather than baking a
// list in, so it does not go stale when a verb is added. Adapted from
// urfave/cli's autocomplete/bash_autocomplete (MIT).
//
// -o bashdefault -o default let filenames still complete after `play < `.
// urfave's own script adds -o nospace; it is left off here because every
// completion this CLI offers is a whole word, and suppressing the trailing
// space makes each one need an extra keystroke.
const bashCompletion = `# chainsmoke bash completion
_chainsmoke_complete() {
    local cur opts
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"

    if [[ "${cur}" == -* ]]; then
        opts=$( "${COMP_WORDS[@]:0:${COMP_CWORD}}" "${cur}" --generate-bash-completion 2>/dev/null )
    else
        opts=$( "${COMP_WORDS[@]:0:${COMP_CWORD}}" --generate-bash-completion 2>/dev/null )
    fi

    COMPREPLY=( $(compgen -W "${opts}" -- "${cur}") )
    return 0
}
complete -o bashdefault -o default -F _chainsmoke_complete chainsmoke
`

// zshCompletion mirrors the bash one through zsh's completion system. Adapted
// from urfave/cli's autocomplete/zsh_autocomplete (MIT).
const zshCompletion = `#compdef chainsmoke
# chainsmoke zsh completion

_chainsmoke_complete() {
    local -a opts
    local cur
    cur=${words[-1]}

    if [[ "${cur}" == -* ]]; then
        opts=("${(@f)$(${words[@]:0:#words[@]-1} ${cur} --generate-bash-completion 2>/dev/null)}")
    else
        opts=("${(@f)$(${words[@]:0:#words[@]-1} --generate-bash-completion 2>/dev/null)}")
    fi

    if [[ "${opts[1]}" != "" ]]; then
        _describe 'chainsmoke' opts
    else
        _files
    fi
}

compdef _chainsmoke_complete chainsmoke
`
