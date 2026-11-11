import type { Models } from './api'

export const clients = [
  'curl',
  'OpenAI Python',
  'Anthropic Python',
  'Claude Code',
  'OpenCode',
] as const
export type Client = (typeof clients)[number]
const shell = (value: string) => "'" + value.replaceAll("'", "'\"'\"'") + "'"

// Keep aligned with docs/CLIENTS.md. Credentials remain in the shell environment.
export function snippet(client: Client, urls: Models, model: string): string {
  const quoted = JSON.stringify(model)
  const origin = JSON.stringify(urls.router_url)
  const base = JSON.stringify(urls.base_url)
  switch (client) {
    case 'curl':
      return [
        '# Set OLR_API_KEY to the key you just copied.',
        'curl --fail ' + shell(urls.base_url + '/chat/completions') + ' \\',
        '  -H "Authorization: Bearer $OLR_API_KEY" \\',
        "  -H 'Content-Type: application/json' \\",
        '  --data ' +
          shell(
            JSON.stringify({
              model,
              messages: [{ role: 'user', content: 'Hello!' }],
              max_tokens: 128,
            }),
          ),
      ].join('\n')
    case 'OpenAI Python':
      return `import os
from openai import OpenAI

client = OpenAI(base_url=${base}, api_key=os.environ["OLR_API_KEY"])
response = client.chat.completions.create(
    model=${quoted},
    messages=[{"role": "user", "content": "Hello!"}],
    max_tokens=128,
)
print(response.choices[0].message.content)`
    case 'Anthropic Python':
      return `import os
from anthropic import Anthropic

client = Anthropic(base_url=${origin}, api_key=os.environ["OLR_API_KEY"])
response = client.messages.create(
    model=${quoted},
    messages=[{"role": "user", "content": "Hello!"}],
    max_tokens=128,
)
print(response.content[0].text)`
    case 'Claude Code':
      return `export ANTHROPIC_BASE_URL=${shell(urls.router_url)}
export ANTHROPIC_API_KEY="$OLR_API_KEY"
export ANTHROPIC_AUTH_TOKEN="$OLR_API_KEY"
export ANTHROPIC_DEFAULT_SONNET_MODEL=${shell(model)}
export ANTHROPIC_DEFAULT_HAIKU_MODEL=${shell(model)}
export ANTHROPIC_DEFAULT_OPUS_MODEL=${shell(model)}
export MAX_THINKING_TOKENS=0
export CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1
claude --bare --model ${shell(model)}`
    case 'OpenCode':
      return JSON.stringify(
        {
          $schema: 'https://opencode.ai/config.json',
          model: `olr/${model}`,
          small_model: `olr/${model}`,
          provider: {
            olr: {
              npm: '@ai-sdk/openai-compatible',
              name: 'Open LLM Router',
              options: { baseURL: urls.base_url, apiKey: '{env:OLR_API_KEY}' },
              models: {
                [model]: {
                  name: model,
                  limit: { context: 32768, output: 4096 },
                  options: { reasoningEffort: 'none' },
                },
              },
            },
          },
        },
        null,
        2,
      )
  }
}
