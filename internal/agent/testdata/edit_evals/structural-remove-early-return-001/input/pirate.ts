/**
 * Pirate Extension
 *
 * Demonstrates using systemPromptAppend in before_agent_start to dynamically
 * modify the system prompt based on extension state.
 *
 * Usage:
 * 1. Copy this file to ~/.omp/agent/extensions/ (legacy: ~/.pi/agent/extensions/) or your project's .omp/extensions/
 * 2. Use /pirate to toggle pirate mode
 * 3. When enabled, the agent will respond like a pirate
 */
import type { ExtensionAPI } from '@oh-my-pi/pi-coding-agent';

export default function pirateExtension(pi: ExtensionAPI) {
  let pirateMode = false;

  // Register /pirate command to toggle pirate mode
  pi.registerCommand('pirate', {
    description: 'Toggle pirate mode (agent speaks like a pirate)',
    handler: async (_args, ctx) => {
      pirateMode = !pirateMode;
      ctx.ui.notify(pirateMode ? 'Arrr! Pirate mode enabled!' : 'Pirate mode disabled', 'info');
    },
  });

  // Append to system prompt when pirate mode is enabled
  pi.on('before_agent_start', async () => {
    return undefined;
  });
}
