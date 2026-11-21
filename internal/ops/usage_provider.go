package ops

import "strings"

// UsageProvider maps known provider spellings to the public label and stable
// identity namespace. Harness names and generic OpenAI-compatible proxies do
// not prove a provider. Custom namespaces retain their spelling.
func UsageProvider(name string) string {
	name = strings.TrimSpace(name)
	switch strings.ToLower(name) {
	case "openai", "open ai", "openai api":
		return "OpenAI"
	case "anthropic", "anthropic ai":
		return "Anthropic"
	case "google", "google ai", "google gemini":
		return "Google"
	case "openrouter", "open router", "openrouter.ai":
		return "OpenRouter"
	case "deepseek", "deep seek", "deepseek-ai":
		return "DeepSeek"
	case "amazon bedrock", "aws bedrock":
		return "Amazon Bedrock"
	case "opencode go":
		return "OpenCode Go"
	case "opencode zen":
		return "OpenCode Zen"
	default:
		return name
	}
}
