package main

import (
	"embed"
	"html/template"
	"net/http"
)

//go:embed web/*.mjs
var consoleScripts embed.FS

var consolePage = template.Must(template.New("console").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Hermit SQL console</title>
<style>
:root{font-family:ui-sans-serif,system-ui;color:#26332e;background:#f7f5ee}body{max-width:900px;margin:3rem auto;padding:0 1rem}header{display:flex;align-items:center;gap:1rem}h1{margin:0}p{color:#596861}label{display:block;font-weight:600;margin:1rem 0 .35rem}input,textarea,button{font:inherit;box-sizing:border-box}input,textarea{width:100%;padding:.7rem;border:1px solid #b7c3b9;border-radius:8px;background:white}textarea{min-height:140px;font-family:ui-monospace,monospace}button{background:#275c49;color:white;border:0;border-radius:8px;padding:.75rem 1.2rem;cursor:pointer;margin-right:.5rem;margin-top:1rem}button:disabled{opacity:.55;cursor:wait}pre{overflow:auto;background:#172820;color:#e4f1e8;padding:1rem;border-radius:8px;min-height:4rem}small{color:#596861}
</style><header><span style="font-size:3rem" aria-hidden="true">🦀</span><div><h1>Hermit</h1><p>PostgreSQL wire protocol, carried in a WebSocket home.</p></div></header>
<form id="query-form"><label for="connection">PostgreSQL connection string</label><input id="connection" type="password" autocomplete="off" placeholder="postgres://postgres:postgres@localhost:5432/postgres"><small>Use a username and password for WebSocket. HTTP also accepts a bearer token. The host is always the configured upstream.</small>
<label for="token">OAuth/JWT bearer token</label><input id="token" type="password" autocomplete="off" placeholder="Optional; sent to PostgreSQL as the password"><small>HTTP only. WebSocket uses the username and password above.</small><label for="sql">SQL</label><textarea id="sql">select now() as server_time, current_user as db_user;</textarea><button type="submit" name="mode" value="http">Run via HTTP</button><button type="submit" name="mode" value="ws">Run via WebSocket</button></form><h2>Result</h2><pre id="result">Ready.</pre>
<script type="module" src="/console.mjs"></script></html>`))

func console(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = consolePage.Execute(w, nil)
}

func consoleScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFileFS(w, r, consoleScripts, "web/"+r.URL.Path[1:])
}
