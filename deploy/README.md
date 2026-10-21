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
machine you run it on. It needs an elevated PowerShell session (it installs
and removes a real `bento-ads` Windows service) and aborts if a `bento-ads`
service already exists, so as not to touch a real install:

```powershell
.\deploy\test-remote-blocks.ps1
```
