package web

import "embed"

//go:embed setup_static/*
var embeddedSetup embed.FS
