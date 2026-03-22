#!/usr/bin/env sh
set -eu

if [ $# -lt 1 ]; then
  echo "usage: ./import-image.sh <docker-image-tar>"
  exit 1
fi

docker load -i "$1"
echo "Image imported. Copy ai-gateway.env.example to ai-gateway.env before running docker compose up -d."

