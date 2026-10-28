# Upgrading

A deployment runs the release image of the binary that rendered it. To move
to a newer release, upgrade the binary, then the deployment:

```sh
brew upgrade triagefactory
triagefactory deploy upgrade
```

Until you run `deploy upgrade`, `deploy up` refuses to start a deployment
older than the binary. Releases only go forward: a binary older than the
deployment is refused too, so install the deployment's version instead.

`deploy upgrade` checks everything it can before it touches the running stack.
If any check fails, nothing has changed:

- the new image's signature;
- the rendered files, which must be unchanged since `deploy` wrote them;
- every value the new release needs, which `.env` must have or `deploy` must
  be able to generate;
- the new image under the new seccomp profile.

Then it drains the executors, waits up to `--drain-timeout` (default `10m`) for
their runs to finish, re-renders the compose files and restarts the stack on
the new image. `control` migrates the database as it boots. A run still going
when the timeout ends is saved with a snapshot of its workspace and continues
on the upgraded executors. Interrupting the upgrade during the drain (Ctrl-C)
undrains the executors and changes nothing.

**It is not a rolling upgrade.** The whole stack restarts, the API is down until
`control` is back, and every executor is replaced at once. Upgrade in a quiet
window.

If you edited a rendered compose file, the upgrade lists it and stops. Move
the change to `compose.override.yml` and run it again, or pass `--force` to
overwrite the file.
