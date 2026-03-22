param(
  [string]$Version = "0.1.5"
)

$ErrorActionPreference = "Stop"

$root = Resolve-Path (Join-Path $PSScriptRoot "..")
$dist = Join-Path $root "dist"
$bundle = Join-Path $dist "bundle"
$imageTag = "ai-gateway:$Version"
$binaryName = "ai-gateway_${Version}_linux_amd64"
$binaryTar = Join-Path $dist "${binaryName}.tar.gz"
$imageTar = Join-Path $dist "ai-gateway_${Version}_docker-image.tar"
$sourceZip = Join-Path $dist "ai-gateway_${Version}_source.zip"
$bundleZip = Join-Path $dist "ai-gateway_${Version}_airgap-bundle.zip"
$checksums = Join-Path $dist "ai-gateway_${Version}_checksums.txt"

if (Test-Path $dist) {
  Remove-Item -Recurse -Force $dist
}

New-Item -ItemType Directory -Path $dist | Out-Null
New-Item -ItemType Directory -Path $bundle | Out-Null

Write-Host "Building Linux binary with Docker..."
docker build --target build -t ai-gateway-builder:$Version $root | Out-Host
$containerId = docker create ai-gateway-builder:$Version
docker cp "${containerId}:/out/ai-gateway" (Join-Path $dist "ai-gateway")
docker rm $containerId | Out-Null
tar -czf $binaryTar -C $dist ai-gateway
Remove-Item (Join-Path $dist "ai-gateway")

Write-Host "Building runtime image..."
docker build -t $imageTag $root | Out-Host
docker save -o $imageTar $imageTag

Write-Host "Creating source archive..."
git -C $root archive --format zip --output $sourceZip HEAD

Write-Host "Collecting air-gap bundle files..."
Copy-Item -Recurse -Force (Join-Path $root "deploy\\airgap\\*") $bundle
Copy-Item -Force $binaryTar $bundle
Copy-Item -Force $imageTar $bundle
Copy-Item -Force $sourceZip $bundle
Copy-Item -Force (Join-Path $root "README.md") $bundle

Compress-Archive -Path (Join-Path $bundle "*") -DestinationPath $bundleZip

$hashLines = @(
  (Get-FileHash $binaryTar -Algorithm SHA256 | ForEach-Object { "$($_.Hash)  $(Split-Path $_.Path -Leaf)" }),
  (Get-FileHash $imageTar -Algorithm SHA256 | ForEach-Object { "$($_.Hash)  $(Split-Path $_.Path -Leaf)" }),
  (Get-FileHash $sourceZip -Algorithm SHA256 | ForEach-Object { "$($_.Hash)  $(Split-Path $_.Path -Leaf)" }),
  (Get-FileHash $bundleZip -Algorithm SHA256 | ForEach-Object { "$($_.Hash)  $(Split-Path $_.Path -Leaf)" })
)
$hashLines | Set-Content $checksums

Write-Host "Done."
Write-Host "Artifacts:"
Write-Host " - $binaryTar"
Write-Host " - $imageTar"
Write-Host " - $sourceZip"
Write-Host " - $bundleZip"
Write-Host " - $checksums"
