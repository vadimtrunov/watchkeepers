// @ts-check
import js from "@eslint/js";
import prettier from "eslint-config-prettier";
import importX from "eslint-plugin-import-x";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
  {
    ignores: [
      "**/dist/**",
      "**/coverage/**",
      "**/node_modules/**",
      "bin/**",
      ".omc/**",
      ".claude/**",
    ],
  },

  // Baseline recommended rules for every file.
  js.configs.recommended,

  // TypeScript — strict + stylistic with type-aware linting.
  {
    files: ["**/*.ts", "**/*.tsx"],
    extends: [...tseslint.configs.strictTypeChecked, ...tseslint.configs.stylisticTypeChecked],
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
      globals: {
        ...globals.node,
      },
    },
    plugins: {
      "import-x": importX,
    },
    rules: {
      "@typescript-eslint/no-floating-promises": "error",
      "@typescript-eslint/no-misused-promises": "error",
      "@typescript-eslint/switch-exhaustiveness-check": "error",
      "@typescript-eslint/consistent-type-imports": "error",
      "import-x/order": [
        "error",
        {
          "newlines-between": "always",
          alphabetize: { order: "asc", caseInsensitive: true },
          groups: ["builtin", "external", "internal", "parent", "sibling", "index"],
        },
      ],
      "no-restricted-imports": [
        "error",
        {
          paths: [
            ...["fs", "fs/promises", "node:fs", "node:fs/promises"].map((name) => ({
              name,
              message:
                "Direct fs access is gated. Declare an fs capability and go through the capability broker (see ROADMAP M5).",
            })),
            ...[
              "http",
              "https",
              "http2",
              "net",
              "tls",
              "dgram",
              "dns",
              "dns/promises",
              "node:http",
              "node:https",
              "node:http2",
              "node:net",
              "node:tls",
              "node:dgram",
              "node:dns",
              "node:dns/promises",
            ].map((name) => ({
              name,
              message:
                "Direct network access is gated. Declare a net capability and go through the capability broker (see ROADMAP M5).",
            })),
            ...["child_process", "node:child_process"].map((name) => ({
              name,
              message:
                "Subprocess execution is gated. Declare a proc capability and go through the capability broker (see ROADMAP M5).",
            })),
          ],
        },
      ],
    },
  },

  // Test files relax the capability gate so tests can stub builtin modules.
  {
    files: ["**/*.test.ts", "**/*.test.tsx", "**/test/**/*.ts"],
    rules: {
      "no-restricted-imports": "off",
    },
  },

  // Plain JS config files — disable type-aware rules (they are not in tsconfig).
  {
    files: ["**/*.{js,mjs,cjs}"],
    extends: [tseslint.configs.disableTypeChecked],
  },

  // Prettier must go last so it can turn off formatting rules.
  prettier,
);
