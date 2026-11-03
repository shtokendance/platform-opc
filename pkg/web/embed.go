package web

import "embed"

//go:embed admin.html nav.html deploy_admin.html *.js
var AdminFS embed.FS
