param(
  [Parameter(Mandatory = $true)]
  [string]$ImageTar
)

docker load -i $ImageTar
Write-Host "Image imported. Copy ai-gateway.env.example to ai-gateway.env before running docker compose up -d."

