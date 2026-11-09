# smrit

The Smrit command line. It signs in to Smrit and moves a folder of docs into it. It is built for agents as much as for people: when its output is not a terminal it writes JSON, and `smrit help --json` describes every command, flag and exit code.

Documentation: coming soon

## Install

```
go install github.com/smrit-ai/cli/cmd/smrit@latest
```

This needs Go 1.25 or newer. A one line installer and prebuilt binaries are coming with the first release.

## Commands

| Command | What it does |
|---|---|
| `smrit login` | Sign in through your browser. |
| `smrit logout` | Sign out and forget the sign in on this machine. |
| `smrit status` | Show who you are signed in as and what you can see. |
| `smrit call <tool>` | Call any Smrit tool. Pass arguments with `--arg name=value` (repeatable, JSON values are sent as JSON) or as one object with `--json-args '{...}'`. |
| `smrit scan <path>...` | List the files in folders with their git state, links and flags, never their contents. |
| `smrit import <plan>` | Import a plan into Smrit. `--dry-run` previews it and writes nothing. `--resume <run>` carries on a run, skipping what it already imported. |
| `smrit retire <run>` | Remove the files a finished import moved into Smrit, zipping any that git cannot restore. Without `--apply` it only reports. |
| `smrit upgrade` | Replace this smrit with the latest release, verified against its checksum. `--check` only reports the latest version. |
| `smrit help --json` | The whole command tree as JSON. |

`--json` forces JSON output in a terminal. `--org` chooses the organization when you are signed in to several.

## How a move works

1. `smrit scan` lists what is in your docs folder, with git state, links and flags, and never reads the contents out.
2. An agent, or you, writes a plan: which files become documents, workbooks or objects, in which space, with which type.
3. `smrit import <plan> --dry-run` shows what would happen, in your organization, and writes nothing.
4. `smrit import <plan>` does it, and can be resumed if it stops part way.
5. `smrit retire <run>` removes the originals that are now safely in Smrit, and only with `--apply`.

## What it will not do

- Import a file that looks like it holds a secret, or whose name says it does. There is no override.
- Scan your home folder, anything above it, or a hidden folder.
- Write to a public space.
- Delete a README, a licence, an agent instruction file, or a file a build or a docs site uses.
- Delete anything that is not safely in Smrit, in the zip, or in git.
- Talk to any server but Smrit's own, and GitHub for `smrit upgrade` alone. Every address is built in; the hosts are listed under Security.

## Sign in and credentials

`smrit login` opens your browser and signs in. The credentials are kept in the macOS keychain, or in a file only you can read where there is no keychain, and are never printed. For scripts and agents without a browser, set `SMRIT_TOKEN`.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | The command did what was asked. |
| 1 | The command ran but some items failed or were skipped. |
| 2 | The command line was wrong: an unknown command or flag, or a missing argument. |
| 3 | There is no usable sign in; run `smrit login`. |
| 4 | The request was refused, by the server (permissions or limits) or by smrit (a folder it will not scan, or an import plan in which every entry is refused). |
| 5 | The server could not be reached or failed; try again later. |
| 6 | This version of smrit is too old for the server; install a newer release. |

## Security

The hosts the CLI connects to are built in, and nothing in a release build changes them:

| Host | What for |
|---|---|
| `mcp.smrit.ai` | Signing in, every Smrit tool call, and importing documents. Your credentials are sent here and nowhere else. |
| `trove.smrit.ai` | Uploading the files an import stores as objects, each to an address Smrit gives for that one file. No credential of yours goes with them. |
| `github.com`, `objects.githubusercontent.com`, `release-assets.githubusercontent.com` | `smrit upgrade` only: the latest release, its checksums and its binary, from the `smrit-ai/cli` releases. No credential of yours goes with them. |

`smrit login` also opens Smrit's sign in page in your browser, and listens on 127.0.0.1, on your own machine, for the browser to come back.

To report a vulnerability, email security@smrit.ai. Please do not open a public issue for it.

## Licence

MIT. See [LICENSE](LICENSE).
