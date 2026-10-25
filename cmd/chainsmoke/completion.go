package main

import (
	"fmt"
	"os"

	"github.com/urfave/cli/v2"
)

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
