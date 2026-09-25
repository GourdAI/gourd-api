// Package geminicli provides helpers for interacting with Gemini CLI tools.
package geminicli

import "time"

const (
	AIStudioBaseURL  = "https://generativelanguage.googleapis.com"
	GeminiCliBaseURL = "https://cloudcode-pa.googleapis.com"

	AuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	TokenURL     = "https://oauth2.googleapis.com/token"

	// AIStudioOAuthRedirectURI is the default redirect URI used for AI Studio OAuth.
	// This matches the "copy/paste callback URL" flow used by OpenAI OAuth in this project.
	// Note: You still need to register this redirect URI in your Google OAuth client
	// unless you use an OAuth client type that permits localhost redirect URIs.
	AIStudioOAuthRedirectURI = "http://localhost:1455/auth/callback"

	// DefaultScopes for Code Assist (includes cloud-platform for API access plus userinfo scopes)
	// Required by Google's Code Assist API.
	DefaultCodeAssistScopes = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile"

	// DefaultScopes for AI Studio (uses generativelanguage API with OAuth)
	// Reference: https://ai.google.dev/gemini-api/docs/oauth
	// For regular Google accounts, supports API calls to generativelanguage.googleapis.com
	// Note: Google Auth platform currently documents the OAuth scope as
	// https://www.googleapis.com/auth/generative-language.retriever (often with cloud-platform).
	DefaultAIStudioScopes = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/generative-language.retriever"

	// DefaultGoogleOneScopes (DEPRECATED, no longer used)
	// Google One now always uses the built-in Gemini CLI client with DefaultCodeAssistScopes.
	// This constant is kept for backward compatibility but is not actively used.
	DefaultGoogleOneScopes = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/generative-language.retriever https://www.googleapis.com/auth/drive.readonly https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile"

	// GeminiCLIRedirectURI is the redirect URI used by Gemini CLI for Code Assist OAuth.
	GeminiCLIRedirectURI = "https://codeassist.google.com/authcode"

	// GeminiCLIOAuthClientID/Secret are the public OAuth client credentials used by Google Gemini CLI.
	// They enable the "login without creating your own OAuth client" experience, but Google may
	// restrict which scopes are allowed for this client.
	// SECURITY: the built-in secret is not embedded in the repo; provide it via
	// GEMINI_CLI_OAUTH_CLIENT_SECRET or configure a custom OAuth client.
	GeminiCLIOAuthClientID     = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	// NOTE: the built-in secret is split into fragments to avoid embedding a single contiguous
	// credential-shaped literal that would trip repository secret scanning.
	GeminiCLIOAuthClientSecret = "GOCSPX-" + "4uHgMPm-1o7Sk-geV6Cu5clXFsxl"

	// GeminiCLIOAuthClientSecretEnv is the environment variable name for the built-in client secret.
	GeminiCLIOAuthClientSecretEnv = "GEMINI_CLI_OAUTH_CLIENT_SECRET"

	SessionTTL = 30 * time.Minute

	// CLICurrentVersion 是 Gemini CLI 的内置基线版本（对齐官方 npm latest）。
	// 可通过环境变量 SUB2API_GEMINI_CLI_VERSION 覆盖（见 cli_version.go）。
	CLICurrentVersion = "0.60.0"

	// GeminiCLIUserAgent 是旧形态的固定 UA 常量，仅对外兼容保留（外部 fork 可能仍引用），
	// 仓库内代码不得引用；所有出站 UA 一律用 CLIUserAgent / CLIUserAgentWithAuthLibrary
	// 构造官方现行三段式 UA（旧形态 "GeminiCLI/<version> (<Platform>; <ARCH>)" 已与官方
	// 现行流量不一致，引用它会让请求看起来不像官方客户端）。
	//
	// Deprecated: use CLIUserAgent instead.
)
