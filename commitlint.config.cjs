/**
 * Watchkeeper commit message policy — conventional commits.
 * Applies in CI (PR title + every commit on the PR) and locally via lefthook.
 */
module.exports = {
  extends: ["@commitlint/config-conventional"],
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
