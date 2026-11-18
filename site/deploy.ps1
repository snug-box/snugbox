# Builds snugbox.dev and makes the build live on the server.
#
#   pwsh -File site/deploy.ps1 [-Server user@host] [-Key path\to\ssh-key]
#
# Without parameters it reads Server and Key from deploy.config.psd1 next to
# this script, which git ignores:  @{ Server = "user@host"; Key = "C:\path\key" }
# To go back to the release before, run `sudo snugbox-site rollback` on the server.
param([string]$Server, [string]$Key)
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
$config = Join-Path $PSScriptRoot "deploy.config.psd1"
if ((-not $Server -or -not $Key) -and (Test-Path $config)) {
    $c = Import-PowerShellDataFile $config
    if (-not $Server) { $Server = $c.Server }
    if (-not $Key) { $Key = $c.Key }
}
if (-not $Server -or -not $Key) { throw "give -Server and -Key, or write them to $config" }

py -3 build.py
if ($LASTEXITCODE) { throw "build.py failed" }
tar -czf site.tgz -C public .
if ($LASTEXITCODE) { throw "tar failed" }
$opts = @("-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-i", $Key)
try {
    scp @opts site.tgz "${Server}:/tmp/snugbox-site.tgz"
    if ($LASTEXITCODE) { throw "upload failed" }
    ssh @opts $Server "sudo snugbox-site deploy /tmp/snugbox-site.tgz; rc=`$?; rm -f /tmp/snugbox-site.tgz; exit `$rc"
    if ($LASTEXITCODE) { throw "deploy failed" }
}
finally {
    Remove-Item site.tgz -ErrorAction SilentlyContinue
}
