package web

import "embed"

//go:embed miniapp_dist/*
var embeddedDist embed.FS
