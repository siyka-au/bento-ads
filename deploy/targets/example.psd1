@{
    # SSH target. KeyFile is passed to ssh/scp with -i -o IdentitiesOnly=yes,
    # so it doesn't have to be the only key ssh-agent would offer.
    Host    = '203.0.113.10'
    User    = 'Administrator'
    KeyFile = '~/.ssh/id_ed25519_example'

    # Where bento-ads lives on the target. Default if omitted: C:\bento.
    RemoteDir = 'C:\bento'

    # Local paths, relative to the repo root. ConfigFile deploys as
    # RemoteDir\<its own file name>; EnvFile always deploys as RemoteDir\.env
    # (whatever it's called locally); each CertFiles entry deploys as
    # RemoteDir\certs\<its own file name>.
    ConfigFile = 'example/config.yaml'
    EnvFile    = '.env.example-target'
    CertFiles  = @()

    # sc.exe failure actions: restart after RestartDelaySec1, then
    # RestartDelaySec2, then RestartDelaySec3 on repeated failures; the
    # failure count resets after ResetDays with no further failures.
    # Omit ServiceRecovery entirely to take these defaults.
    ServiceRecovery = @{
        RestartDelaySec1 = 60
        RestartDelaySec2 = 60
        RestartDelaySec3 = 300
        ResetDays        = 1
    }

    # Service name as installed by `bento-ads.exe service install` --
    # hardcoded to "bento-ads" in cmd/bento-ads/main.go, so there's normally
    # nothing to set here. deploy/test-remote-blocks.ps1 reads this same
    # field so it can refuse to run against a real "bento-ads" service.
    # ServiceName = 'bento-ads'
}
