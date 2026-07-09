#!/usr/bin/env node
// redactor.mjs — reference harness plugin built on the TypeScript/JS SDK.
//
// Demonstrates:
//   - tool.execute.after: redacts secret-looking substrings from any tool's
//     output before it reaches the model.
//   - a custom tool (echo_reverse) added to the model's tool list.
//
// Run standalone for a quick manual smoke test:
//   node examples/plugins/redactor.mjs
// (it will then wait for NDJSON on stdin, as any harness plugin does).
//
// See sdk/typescript/README.md for the full walkthrough.

import { createPlugin } from '../../sdk/typescript/harness-plugin.mjs';

// Patterns of things that look like secrets. Deliberately simple and
// dependency-free; real plugins can compile whatever patterns they need.
const SECRET_PATTERNS = [
  /sk-[A-Za-z0-9]{16,}/g, // OpenAI-style API keys
  /ghp_[A-Za-z0-9]{20,}/g, // GitHub personal access tokens
  /AKIA[0-9A-Z]{16}/g, // AWS access key IDs
];

/** Redacts secret-looking substrings from a string. */
function redact(text) {
  let out = text;
  for (const pattern of SECRET_PATTERNS) {
    out = out.replace(pattern, '[REDACTED]');
  }
  return out;
}

/** Redacts secrets from every text part of a message.Parts array. */
function redactParts(parts) {
  let changed = false;
  const out = parts.map((part) => {
    if (part.type === 'text' && typeof part.text === 'string') {
      const redacted = redact(part.text);
      if (redacted !== part.text) changed = true;
      return { ...part, text: redacted };
    }
    return part;
  });
  return { out, changed };
}

createPlugin({
  name: 'redactor',
  version: '0.1.0',
  hooks: {
    // Additive: every plugin subscribed to system.transform gets its
    // segment appended, in config order.
    systemTransform: async () => ({
      segments: ['Secrets in tool output are automatically redacted by the redactor plugin.'],
    }),

    // Runs after every tool call across the session (any tool, not just
    // ours) — this is what makes redaction a cross-cutting concern rather
    // than something every tool has to remember to do itself.
    toolExecuteAfter: async (_ctx, req) => {
      const { out, changed } = redactParts(req.output || []);
      if (!changed) return undefined; // no changes: let the chain continue untouched
      return { output: out };
    },
  },
  tools: [
    {
      def: {
        name: 'echo_reverse',
        description: 'Reverses the given text and echoes it back.',
        inputSchema: {
          type: 'object',
          properties: {
            text: { type: 'string', description: 'Text to reverse.' },
          },
          required: ['text'],
        },
      },
      execute: async (_ctx, args) => {
        const text = (args && args.text) || '';
        const reversed = [...text].reverse().join('');
        return [{ type: 'text', text: reversed }];
      },
    },
  ],
}).run();
