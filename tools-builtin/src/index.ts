/**
 * Watchkeeper built-in tools registry — placeholder.
 *
 * Real tools land in M9 (Multi-source Tool Registry) per
 * docs/ROADMAP-phase1.md.
 */

export interface BuiltinTool {
  readonly name: string;
  readonly version: string;
}

export const BUILTIN_TOOLS: readonly BuiltinTool[] = Object.freeze([]);
