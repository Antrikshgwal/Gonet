package gonet

import "embed"

//go:embed all:client
var ClientFS embed.FS

//go:embed all:site
var SiteFS embed.FS
