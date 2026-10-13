package sandbox

import "testing"

func TestADockerfileBecomesABaseImageAndAListOfSteps(t *testing.T) {
	df, err := parseDockerfile(`
# the project's toolchain
ARG GOVERSION=1.24
FROM debian:stable-slim
ENV LANG=C.UTF-8
ENV PATH /usr/local/go/bin:/usr/bin:/bin
WORKDIR /tmp
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      build-essential git
COPY setup.sh /usr/local/bin/setup
RUN /usr/local/bin/setup ${GOVERSION}
`)
	if err != nil {
		t.Fatal(err)
	}
	if df.From != "debian:stable-slim" {
		t.Errorf("FROM = %q", df.From)
	}
	if len(df.Env) != 2 || df.Env[0] != "LANG=C.UTF-8" || df.Env[1] != "PATH=/usr/local/go/bin:/usr/bin:/bin" {
		t.Errorf("ENV = %q", df.Env)
	}
	if len(df.Steps) != 3 {
		t.Fatalf("steps = %+v", df.Steps)
	}

	// The continuation folded into one command, and it runs with the ENVs
	// above it and in the WORKDIR that was current.
	if got := df.Steps[0].Cmd; got != "apt-get update &&  apt-get install -y --no-install-recommends  build-essential git" {
		t.Errorf("RUN = %q", got)
	}
	if df.Steps[0].Workdir != "/tmp" || len(df.Steps[0].Env) != 2 {
		t.Errorf("step context = %+v", df.Steps[0])
	}
	if df.Steps[1].Op != "copy" || df.Steps[1].Dst != "/usr/local/bin/setup" {
		t.Errorf("COPY = %+v", df.Steps[1])
	}
	// ARG reached the RUN that used it.
	if df.Steps[2].Cmd != "/usr/local/bin/setup 1.24" {
		t.Errorf("ARG expansion = %q", df.Steps[2].Cmd)
	}
}

// An instruction john cannot carry out is named, not dropped. A silently
// ignored USER means the build ran as root when the file said otherwise, and a
// silently ignored second FROM means the environment is a different image than
// the one the developer wrote down.
func TestADockerfileJohnCannotBuildSaysWhichLine(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"second FROM", "FROM a\nRUN x\nFROM b", "a second FROM on line 3"},
		{"unsupported", "FROM a\nUSER nobody", "line 2: john does not build USER"},
		{"cross-stage copy", "FROM a\nCOPY --from=build /x /y", "line 2: COPY --from"},
		{"no FROM", "RUN x", "no FROM"},
		{"short COPY", "FROM a\nCOPY x", "line 2: COPY needs a source"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseDockerfile(c.text)
			if err == nil {
				t.Fatal("accepted a Dockerfile john cannot build")
			}
			if !contains(err.Error(), c.want) {
				t.Fatalf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
