// `sift completion bash|zsh|fish` (issue #935): static completion scripts
// generated from the same command metadata table as help, so Tab completion
// can never drift from the documented command/flag surface. Static by design:
// dynamic run-id/project-id completion is explicitly out of scope.
package main

import (
	"fmt"
	"io"
	"strings"
)

// runCompletion implements `sift completion <shell>`. With no argument it
// prints the install instructions (`eval "$(sift completion zsh)"` or writing
// the script into the shell's rc/completions directory).
func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printCompletionInstructions(stdout)
		return 0
	}
	if len(args) != 1 {
		report(stderr, fmt.Errorf("usage: sift completion bash|zsh|fish"))
		return 2
	}
	switch args[0] {
	case "bash":
		writeBashCompletion(stdout)
	case "zsh":
		writeZshCompletion(stdout)
	case "fish":
		writeFishCompletion(stdout)
	default:
		report(stderr, fmt.Errorf("不支持的 shell %q；支持 bash、zsh、fish", args[0]))
		return 2
	}
	return 0
}

func printCompletionInstructions(w io.Writer) {
	fmt.Fprintln(w, "Sift shell 补全")
	fmt.Fprintln(w, "\nbash：")
	fmt.Fprintln(w, "  source <(sift completion bash)                    # 当前会话")
	fmt.Fprintln(w, "  sift completion bash > ~/.bash_completion.d/sift   # 永久（按发行版 bash-completion 配置加载）")
	fmt.Fprintln(w, "\nzsh：")
	fmt.Fprintln(w, "  eval \"$(sift completion zsh)\"                    # 当前会话（需先 autoload -U compinit; compinit）")
	fmt.Fprintln(w, "  sift completion zsh > \"${fpath[1]}/_sift\"         # 永久（fpath 需包含 compinit 目录）")
	fmt.Fprintln(w, "\nfish：")
	fmt.Fprintln(w, "  sift completion fish > ~/.config/fish/completions/sift.fish")
	fmt.Fprintln(w, "\n重启 shell 后 Tab 即可补全命令与参数。")
}

// writeBashCompletion emits a bash script with a complete -F handler: the
// first word completes the command list, every following word completes the
// flags (and action verbs where a command has them) of the command.
func writeBashCompletion(w io.Writer) {
	fmt.Fprintln(w, "# bash completion for sift")
	fmt.Fprintln(w, "# Install: source <(sift completion bash)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "_sift() {")
	fmt.Fprintln(w, "    local cur")
	fmt.Fprintln(w, "    cur=\"${COMP_WORDS[COMP_CWORD]}\"")
	fmt.Fprintln(w, "    if [ \"${COMP_CWORD}\" -eq 1 ]; then")
	fmt.Fprintf(w, "        COMPREPLY=( $(compgen -W \"%s\" -- \"${cur}\") )\n", strings.Join(commandNames(), " "))
	fmt.Fprintln(w, "        return 0")
	fmt.Fprintln(w, "    fi")
	fmt.Fprintln(w, "    case \"${COMP_WORDS[1]}\" in")
	for _, m := range commands {
		words := append(append([]string{}, m.subcommands...), m.flagWords()...)
		if len(words) == 0 {
			continue
		}
		fmt.Fprintf(w, "        %s)\n", m.name)
		fmt.Fprintf(w, "            COMPREPLY=( $(compgen -W \"%s\" -- \"${cur}\") )\n", strings.Join(words, " "))
		fmt.Fprintln(w, "            ;;")
	}
	fmt.Fprintln(w, "    esac")
	fmt.Fprintln(w, "    return 0")
	fmt.Fprintln(w, "}")
	fmt.Fprintln(w, "complete -F _sift sift")
}

// writeZshCompletion emits an eval-able / compdef-able zsh completion: command
// names with one-line descriptions at the top level, per-command _arguments
// specs for flags, and action verbs where a command has them.
func writeZshCompletion(w io.Writer) {
	fmt.Fprintln(w, "#compdef sift")
	fmt.Fprintln(w, "# zsh completion for sift")
	fmt.Fprintln(w, "# Install: eval \"$(sift completion zsh)\" or write to fpath as _sift")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "_sift() {")
	fmt.Fprintln(w, "    local -a commands")
	fmt.Fprintln(w, "    commands=(")
	for _, m := range commands {
		fmt.Fprintf(w, "        '%s:%s'\n", m.name, m.summary)
	}
	fmt.Fprintln(w, "    )")
	fmt.Fprintln(w, "    if (( CURRENT == 2 )); then")
	fmt.Fprintln(w, "        _describe 'command' commands")
	fmt.Fprintln(w, "        return")
	fmt.Fprintln(w, "    fi")
	fmt.Fprintln(w, "    case \"${words[2]}\" in")
	for _, m := range commands {
		if len(m.flags) == 0 && len(m.subcommands) == 0 {
			continue
		}
		fmt.Fprintf(w, "        %s)\n", m.name)
		if len(m.subcommands) > 0 {
			fmt.Fprintln(w, "            if (( CURRENT == 3 )); then")
			fmt.Fprintln(w, "                local -a actions")
			fmt.Fprintln(w, "                actions=(")
			for _, s := range m.subcommands {
				fmt.Fprintf(w, "                    '%s:%s'\n", s, m.subDescriptions[s])
			}
			fmt.Fprintln(w, "                )")
			fmt.Fprintln(w, "                _describe 'action' actions")
			fmt.Fprintln(w, "            else")
			writeZshArguments(w, m)
			fmt.Fprintln(w, "            fi")
		} else {
			writeZshArguments(w, m)
		}
		fmt.Fprintln(w, "            ;;")
	}
	fmt.Fprintln(w, "    esac")
	fmt.Fprintln(w, "}")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "if [ \"${funcstack[1]}\" = \"_sift\" ]; then")
	fmt.Fprintln(w, "    _sift \"$@\"")
	fmt.Fprintln(w, "else")
	fmt.Fprintln(w, "    compdef _sift sift")
	fmt.Fprintln(w, "fi")
}

// writeZshArguments emits one _arguments spec per flag. Value flags use the
// `--flag=[desc]` form so the shell accepts both `--flag value` and
// `--flag=value`; boolean flags use `--flag[desc]`. Leading `'N: : '`
// positional slots mirror the already-typed words (command verb, action verb,
// usage placeholders) so zsh keeps offering options at positions past the
// first word — without them, comparguments treats the current word as a
// positional and completes nothing (verified against zsh 5.9).
func writeZshArguments(w io.Writer, m commandMeta) {
	if len(m.flags) == 0 {
		return
	}
	slots := 1 // the command verb occupies positional slot 1
	if len(m.subcommands) > 0 {
		slots++ // the action verb (project add / report progress ...)
	}
	slots += strings.Count(m.usage, "<") // explicit placeholders: <run-id>, ...
	specs := make([]string, 0, slots+len(m.flags))
	for i := 1; i <= slots; i++ {
		specs = append(specs, fmt.Sprintf("'%d: : '", i))
	}
	for _, f := range m.flags {
		flag := strings.TrimPrefix(f.flag, "--")
		if f.value == "" {
			specs = append(specs, fmt.Sprintf("'--%s[%s]'", flag, f.desc))
		} else {
			specs = append(specs, fmt.Sprintf("'--%s=[%s]'", flag, f.desc))
		}
	}
	fmt.Fprintln(w, "            _arguments \\")
	for i, s := range specs {
		sep := " \\"
		if i == len(specs)-1 {
			sep = ""
		}
		fmt.Fprintf(w, "                %s%s\n", s, sep)
	}
}

// writeFishCompletion emits fish complete lines: `-f` (no file completion)
// globally, command names under __fish_use_subcommand, action verbs under
// their parent, and per-command flags under `__fish_seen_subcommand_from`.
func writeFishCompletion(w io.Writer) {
	fmt.Fprintln(w, "# fish completion for sift")
	fmt.Fprintln(w, "# Install: sift completion fish > ~/.config/fish/completions/sift.fish")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "complete -c sift -f")
	for _, m := range commands {
		fmt.Fprintf(w, "complete -c sift -n '__fish_use_subcommand' -a '%s' -d '%s'\n", m.name, m.summary)
		for _, s := range m.subcommands {
			fmt.Fprintf(w, "complete -c sift -n '__fish_seen_subcommand_from %s' -a '%s' -d '%s'\n", m.name, s, m.subDescriptions[s])
		}
		seen := "__fish_seen_subcommand_from " + m.name
		if len(m.subcommands) > 0 {
			seen += " " + strings.Join(m.subcommands, " ")
		}
		for _, f := range m.flags {
			argSpec := ""
			if f.value != "" {
				argSpec = " -r"
			}
			fmt.Fprintf(w, "complete -c sift -n '%s' -l %s%s -d '%s'\n", seen, strings.TrimPrefix(f.flag, "--"), argSpec, f.desc)
		}
	}
}
