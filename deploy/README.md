# Deploying bento-ads

`deploy.ps1` builds bento-ads itself, compares it against what's running on a
Windows target over SSH, and -- after you confirm -- stages the changed
files, installs the service (or upgrades it in place), and starts it.

```powershell
.\deploy\deploy.ps1 machine-01
```

It refuses a dirty working tree (`-AllowDirty` to override), refuses to
touch a target that's running `bento-ads.exe` outside the service (it would
fight the service for the MQTT client_id), and asks "remote is X, we are
pushing Y, continue?" before changing anything (`-Yes` to skip). `-Force`
proceeds even if nothing looks different.

Output is six numbered, timed steps -- Build, Inventory, Compare, Stage,
Apply, Verify -- each printing what it actually did, e.g.:

```
[1/6] Build
  building bento-ads.exe...
  built: bento-ads v0.1.0 (commit c08f7d9..., ...)
  done in 3.1s
[4/6] Stage
  -> bento-ads.exe (182.4 MB)
  staged hashes verified (1 files)
  done in 11.2s
[5/6] Apply
  stopped the service (waited 2s for the process to exit)
  backed up current files to C:\bento\backup\20261021-231022
  swapped in: bento-ads.exe
  kept the existing service installation
  started the service
  done in 4.7s
[6/6] Verify
  checked: service running (pid=1234, started 10/21/2026 23:10:27)
  checked: found a 'starting' Event Log entry for the expected version
  checked: 0 ERROR event(s) logged since start
  done in 2.0s
OK: 10.10.106.10 is running bento-ads v0.1.0 (...)
```

Pass `-Verbose` for the `go build -v` output and to see each ssh/scp command
line as it runs (never the encoded payload). Any non-empty stderr from a
remote step (de-noised of CLIXML wrapping) is shown as a yellow warning,
separately from the step's actual result -- it's informational, not a sign
the step failed.

## Target files

Each target is `deploy/targets/<name>.psd1` (gitignored, except
`example.psd1`, the template). Paths inside it are relative to the repo
root.

| Field | Required | Meaning |
|---|---|---|
| `Host`, `User`, `KeyFile` | yes | ssh/scp destination and identity file |
| `RemoteDir` | no (`C:\bento`) | where bento-ads lives on the target |
| `ConfigFile` | yes | local path; deployed as `RemoteDir\<its own name>` |
| `EnvFile` | yes | local path; always deployed as `RemoteDir\.env` |
| `CertFiles` | no | local paths; each deployed as `RemoteDir\certs\<its own name>` |
| `ServiceRecovery` | no | `sc.exe failure` restart delays/reset window; see `example.psd1` |
| `ServiceName` | no (`bento-ads`) | hardcoded in the binary itself; no reason to change |

## First time vs. upgrade

First time (no service installed, or its path doesn't match
`RemoteDir\bento-ads.exe -c RemoteDir\<config> -e RemoteDir\.env`): the
script uninstalls any mismatched service and reinstalls, then applies the
recovery policy and starts it.

Upgrade (service already matches): the binary and any changed files are
swapped in place and the service is restarted -- no reinstall.

Either way, the previous exe/config/.env/certs are backed up to
`RemoteDir\backup\<timestamp>\` first (the 5 most recent backups are kept),
and if the post-start checks fail, the script offers to roll back to that
backup automatically.

## Testing without a real target

`deploy/test-remote-blocks.ps1` runs the same scriptblocks `deploy.ps1`
ships over SSH, but calls them directly against a temp directory on the
machine you run it on. It also runs the exact transport wrapper
(`New-RemoteBlockScript`/`ConvertFrom-RemoteBlockOutput`) through a real
Windows PowerShell 5.1 child process (no ssh needed for that one) to check
that a block's result still parses even when something writes to the error
stream. It needs an elevated PowerShell session (it installs and removes a
real `bento-ads` Windows service) and aborts if a `bento-ads` service
already exists, so as not to touch a real install:

```powershell
.\deploy\test-remote-blocks.ps1
```
