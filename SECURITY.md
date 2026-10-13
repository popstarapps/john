# Security

John is a security boundary, so a flaw in it matters more than most bugs. Thank
you for taking the time to report one.

## Reporting a vulnerability

Report it privately through GitHub: on this repository's **Security** tab,
choose **Report a vulnerability**. Please do not open a public issue.

A useful report says which part is affected — the sandbox, the network proxy,
policy enforcement, authentication, the trace store, the console — what an
attacker can do, and how to reproduce it. A report you are unsure about is
still welcome.

You will get an answer within a few days. Once a fix is released, the advisory
is published, crediting you unless you would rather not be named.

## Supported versions

Only the latest release receives security fixes. The client updates itself to
match its backend, so updating the backend updates everyone.

## Scope

The readme's [Security](readme.md#security) section and threat model describe
what John is meant to hold. Anything that breaks one of those claims is in
scope — for example, a way out of the sandbox, network traffic that avoids the
proxy or a policy, a protected path the agent can read or change, a way around
authentication or authorization, or a trace that can be read or altered without
the key.

Some limits are already documented, such as `-sandbox loopback`, which isolates
nothing and says so, and the known risks listed in the readme. These are not
vulnerabilities unless John fails to behave as documented.
