package sandbox

import (
	"fmt"
	"strings"
)

// The subset of Dockerfile john builds.
//
// This is not a container engine and does not want to be. A development
// environment is a base image and a list of things to install into it, and
// that is what is supported: FROM, RUN, COPY, ENV, WORKDIR and ARG. Those six
// are what a .john/Dockerfile that describes a toolchain actually uses.
//
// Everything else is refused by name rather than ignored. A Dockerfile is a
// familiar format and that familiarity is the reason it was chosen, so the
// dangerous failure is a developer writing an instruction john silently drops
// — a HEALTHCHECK that does nothing is harmless, but a USER that does nothing
// means the build ran as root when the file said otherwise, and a second FROM
// that does nothing means the environment is the wrong image entirely. An
// error naming the line is the only honest answer.
type dockerfile struct {
	// From is the base image reference, as written.
	From string
	// Steps are the build's instructions, in order.
	Steps []buildStep
	// Env is what the finished environment runs with, in order of assignment.
	Env []string
	// Workdir is the directory a command starts in, when the file names one.
	// The workspace is where the agent actually runs, so this only reaches a
	// build's own RUN steps.
	Workdir string
}

type buildStep struct {
	// Op is "run" or "copy".
	Op string
	// Cmd is the shell command, for run.
	Cmd string
	// Src and Dst are the paths, for copy.
	Src []string
	Dst string
	// Env is the environment as it stood at this line, so a RUN sees the ENVs
	// above it and not the ones below.
	Env []string
	// Workdir is the directory this step runs in.
	Workdir string
}

// parseDockerfile reads the project's environment definition.
func parseDockerfile(text string) (*dockerfile, error) {
	df := &dockerfile{}
	args := map[string]string{}

	for _, line := range joinContinuations(text) {
		instr, rest, ok := splitInstruction(line.text)
		if !ok {
			continue
		}
		switch instr {
		case "FROM":
			if df.From != "" {
				return nil, fmt.Errorf("%s: a second FROM on line %d — john builds one "+
					"environment from one base image, and multi-stage builds are a way of "+
					"producing a deployment artifact rather than a place to work",
					EnvFile, line.n)
			}
			df.From = expandArgs(strings.Fields(rest)[0], args)

		case "ARG":
			k, v, _ := strings.Cut(rest, "=")
			args[strings.TrimSpace(k)] = expandArgs(strings.TrimSpace(v), args)

		case "ENV":
			k, v, err := parseAssignment(rest)
			if err != nil {
				return nil, fmt.Errorf("%s line %d: %w", EnvFile, line.n, err)
			}
			df.Env = append(df.Env, k+"="+expandArgs(v, args))

		case "WORKDIR":
			df.Workdir = expandArgs(strings.TrimSpace(rest), args)

		case "RUN":
			df.Steps = append(df.Steps, buildStep{
				Op: "run", Cmd: expandArgs(rest, args),
				Env: append([]string{}, df.Env...), Workdir: df.Workdir,
			})

		case "COPY":
			fields := strings.Fields(expandArgs(rest, args))
			// --from names another build stage, and there is only ever one.
			for _, f := range fields {
				if strings.HasPrefix(f, "--from") {
					return nil, fmt.Errorf("%s line %d: COPY --from refers to another build "+
						"stage, and john builds one", EnvFile, line.n)
				}
			}
			fields = withoutFlags(fields)
			if len(fields) < 2 {
				return nil, fmt.Errorf("%s line %d: COPY needs a source and a destination", EnvFile, line.n)
			}
			df.Steps = append(df.Steps, buildStep{
				Op: "copy", Src: fields[:len(fields)-1], Dst: fields[len(fields)-1],
				Env: append([]string{}, df.Env...), Workdir: df.Workdir,
			})

		default:
			return nil, fmt.Errorf("%s line %d: john does not build %s. The environment is a "+
				"base image and what to install into it: FROM, RUN, COPY, ENV, WORKDIR and ARG",
				EnvFile, line.n, instr)
		}
	}
	if df.From == "" {
		return nil, fmt.Errorf("%s: no FROM — an environment starts from a base image", EnvFile)
	}
	return df, nil
}

type sourceLine struct {
	n    int
	text string
}

// joinContinuations folds backslash-continued lines into one, dropping
// comments and blanks, and keeps the line number the instruction started on
// so an error points where a developer will look.
func joinContinuations(text string) []sourceLine {
	var out []sourceLine
	var buf strings.Builder
	start := 0
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			start = i + 1
		}
		if strings.HasSuffix(trimmed, "\\") {
			buf.WriteString(strings.TrimSuffix(trimmed, "\\"))
			buf.WriteString(" ")
			continue
		}
		buf.WriteString(trimmed)
		out = append(out, sourceLine{start, buf.String()})
		buf.Reset()
	}
	if buf.Len() > 0 {
		out = append(out, sourceLine{start, buf.String()})
	}
	return out
}

func splitInstruction(line string) (string, string, bool) {
	instr, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
	if !ok {
		return strings.ToUpper(instr), "", instr != ""
	}
	return strings.ToUpper(instr), strings.TrimSpace(rest), true
}

// parseAssignment reads `KEY value` and `KEY=value`, which ENV accepts both of.
func parseAssignment(rest string) (string, string, error) {
	if k, v, ok := strings.Cut(rest, "="); ok && !strings.Contains(strings.TrimSpace(k), " ") {
		return strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`), nil
	}
	k, v, ok := strings.Cut(rest, " ")
	if !ok {
		return "", "", fmt.Errorf("ENV needs a name and a value")
	}
	return strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`), nil
}

// expandArgs substitutes $NAME and ${NAME} from ARG. It is deliberately not a
// shell: a RUN's own command is expanded by the shell that runs it, and this
// is only for the values john reads itself.
func expandArgs(s string, args map[string]string) string {
	if len(args) == 0 || !strings.Contains(s, "$") {
		return s
	}
	for k, v := range args {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
		s = strings.ReplaceAll(s, "$"+k, v)
	}
	return s
}

func withoutFlags(fields []string) []string {
	out := fields[:0:0]
	for _, f := range fields {
		if !strings.HasPrefix(f, "--") {
			out = append(out, f)
		}
	}
	return out
}
