# Security

## Report a vulnerability

Please report security problems privately, through GitHub:
[open a security advisory](https://github.com/EledoneAi/murmur/security/advisories/new) on this repository.
Do not open a public issue, pull request or discussion for them.

Say what you found, how to reproduce it, and what an attacker gains. A short proof of concept helps. We aim to
answer within a week, keep you told of progress, and credit you in the advisory unless you ask us not to.

## Supported versions

Murmur is before 1.0. Only the latest release gets security fixes. Self-hosted hubs get a fix by deploying
the release that carries it; laptops then follow their hub as its admins' update policy says, except those
installed with Homebrew or Nix, or set to `murmur updates ask` or `off`, which their people update. Admins can
refuse older laptops by raising the oldest version the hub accepts.

## In scope

Murmur runs on every laptop of a team and on a hub the team hosts, so we want to hear about anything that
crosses one of these lines:

- **Sign-in and sessions on the hub**: getting in without passing the identity provider, getting past the
  allowed domains, emails or GitHub organizations, forging or replaying a session cookie or a laptop sign-in code,
  acting as an admin without being one, a laptop token that keeps working after it was revoked.
- **Who sees what**: message text reaching someone who is not part of the conversation, data leaving a laptop
  beyond what docs/privacy.md lists, one team's hub data reaching another hub.
- **Updates and installs**: a hub, or anyone between the hub and a laptop, getting an installed laptop to update
  to a binary the project's release key did not sign; a downgrade a laptop should have refused; the install script
  installing a binary its hub's `checksums.txt` doesn't list, or doing something other than installing the binary
  and running `murmur join`.
- **The laptop**: another local user reaching the daemon's socket or files under `~/.murmur`, a message from
  another person or agent approving anything on its own, running a command, or changing Claude Code settings.
- **Text from others**: terminal escape codes, control characters or hidden characters in names, titles,
  branches or messages that survive the hub and the laptop's cleaning and reach a terminal or an agent.
- **The hub service**: crashes, memory or connection exhaustion that someone outside the team can cause.

## What teammates and admins can do by design

Murmur links people who trust each other enough to share a hub. These follow from that, and are not bugs:

- A teammate's laptop says what its agents are: their names, repos, branches and the files they change. The hub
  checks who the person is, not what their laptop reports. Mutes on a person always hold; mutes on an agent, a repo
  or a branch go by what the other laptop says.
- A teammate can learn which files other checkouts of a repo change, through the edit-time check.
- Teammates' messages, titles and branches reach your agents as data from other sessions. Claude Code labels them
  so, and they can't approve permissions or change settings, but an agent may still act on what they say: mute
  whoever you don't want your agents to hear.
- An admin's team agreements reach every agent at the start of each session.
- Whoever runs the hub can see everything that passes through it, and decides which version laptops move to. It
  can't make laptops install a binary the release key didn't sign, or move them to an older version.

## Out of scope

- Bugs in Claude Code itself: report them to Anthropic.
- Bugs in an identity provider, a store or a bus (Postgres, Redis, Firestore), a reverse proxy or a hosting
  platform, unless murmur uses it wrongly.
- A hub run against the documentation: for example a guessable `SESSION_SECRET`, or a store or bus reachable
  from the internet without a password.
- A first install trusting its hub: the install script checks the binary against the `checksums.txt` of the hub it
  downloads from, over HTTPS, not against the release signature. Updates after it check the signature.
- Someone the team allowed in doing what a team member can do: seeing who is online, which repos and branches
  agents work on, and messaging people.
- Reports from automated scanners without a way to exploit what they found.
