/**
 * Watchkeeper TypeScript harness — placeholder.
 *
 * Real wiring lands in M5 (Runtime adapter + Claude Code bridge) per
 * docs/ROADMAP-phase1.md.
 */

export interface HarnessBanner {
  readonly name: string;
  readonly placeholder: true;
}

export function banner(): HarnessBanner {
  return { name: "watchkeeper-harness", placeholder: true };
}
