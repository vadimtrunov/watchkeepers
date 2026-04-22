/**
 * Watchkeeper commit message policy — conventional commits.
 * Applies in CI (PR title + every commit on the PR) and locally via lefthook.
 */
module.exports = {
  extends: ["@commitlint/config-conventional"],
  // Pre-existing commits that landed on local main before the commitlint
  // policy existed. Once origin/main is pushed with these commits baked in,
  // they drop out of PR ranges and these ignores can be removed.
  ignores: [
    (message) => /^Add rdd/i.test(message),
    (message) => /^Add Watchkeeper/i.test(message),
    (message) => /^Add \(Roadmap-Driven Development\)/i.test(message),
    (message) => /^Initial commit/i.test(message),
  ],
  rules: {
    "type-enum": [
      2,
      "always",
      [
        "feat",
        "fix",
        "docs",
        "refactor",
        "perf",
        "test",
        "build",
        "ci",
        "chore",
        "revert",
        "style",
      ],
    ],
    "subject-case": [2, "never", ["start-case", "pascal-case", "upper-case"]],
    "header-max-length": [2, "always", 100],
    "body-max-line-length": [1, "always", 120],
    "footer-leading-blank": [2, "always"],
  },
};
