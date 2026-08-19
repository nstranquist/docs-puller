---
title: Install and upgrade
---

# Install and upgrade

Install the local `docs-puller` CLI. It copies vendor and project docs into
Markdown and searches them on your machine.

## Install with Go

Requirement: Go 1.26.6 or later.

```sh
go install github.com/nstranquist/docs-puller@v0.7.2
docs-puller version --expect v0.7.2 --json
docs-puller demo
```

If you want the newest published release instead of a pinned version, use
`@latest`.

If the shell cannot find `docs-puller`, add the Go binary directory to `PATH`.
If `GOBIN` is set, the directory is `$(go env GOBIN)`. Otherwise, it is
`$(go env GOPATH)/bin`.

## Install a release archive

GitHub release archives contain one binary. Select the archive for your
operating system and CPU from the v0.7.2 release page.

On macOS or Linux:

```sh
version="0.7.2"
platform="$(go env GOOS)_$(go env GOARCH)"
archive="docs-puller_${version}_${platform}.tar.gz"
curl --fail --location --remote-name \
  "https://github.com/nstranquist/docs-puller/releases/download/v${version}/${archive}"
curl --fail --location --remote-name \
  "https://github.com/nstranquist/docs-puller/releases/download/v${version}/checksums.txt"
grep "  ${archive}$" checksums.txt | shasum -a 256 --check
tar -xzf "$archive"
./docs-puller version --expect "v${version}" --json
./docs-puller demo
```

After these verification steps pass, move the verified binary to a directory
on `PATH`.

On Windows PowerShell, download the matching `.zip` and `checksums.txt`. Then
verify and extract it:

```powershell
$Version = "0.7.2"
$Archive = "docs-puller_${Version}_windows_amd64.zip"
Invoke-WebRequest "https://github.com/nstranquist/docs-puller/releases/download/v$Version/$Archive" -OutFile $Archive
Invoke-WebRequest "https://github.com/nstranquist/docs-puller/releases/download/v$Version/checksums.txt" -OutFile checksums.txt
$Expected = ((Select-String -Path checksums.txt -Pattern "  $Archive$").Line -split "  ")[0]
$Actual = (Get-FileHash $Archive -Algorithm SHA256).Hash.ToLower()
if ($Actual -ne $Expected) { throw "checksum mismatch" }
Expand-Archive $Archive -DestinationPath .\docs-puller-$Version
.\docs-puller-$Version\docs-puller.exe version --expect "v$Version" --json
.\docs-puller-$Version\docs-puller.exe demo
```

## Install from a checkout

```sh
git clone https://github.com/nstranquist/docs-puller.git
cd docs-puller
make install
docs-puller version --json
```

If `docs-puller` is on your `PATH`, `ndev docs` uses the same CLI.

## Upgrade

Repeat the selected install method with the new SemVer tag. After the upgrade,
run `version --expect` and `demo`. An upgrade does not delete your corpus,
configuration, query log, or embedding index.

If you want to remove those paths, see [Uninstall](uninstall.md).
