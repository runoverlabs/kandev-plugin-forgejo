// Lint gate for the browser bundle. The thresholds are documented, with the
// reasons, in docs/architecture.md; change that page in the same commit.
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["ui/bundle.js", "node_modules/**"] },
  {
    files: ["ui/src/**/*.ts", "ui/test/**/*.ts"],
    extends: [tseslint.configs.recommended],
  },
  {
    files: ["ui/src/**/*.ts"],
    rules: {
      complexity: ["error", 12],
      "max-depth": ["error", 4],
      "max-params": ["error", 4],
      "max-lines-per-function": ["error", { max: 100, skipBlankLines: true, skipComments: true }],
      "max-lines": ["error", { max: 400, skipBlankLines: true, skipComments: true }],
    },
  },
  {
    // The tests stub the host loosely on purpose.
    files: ["ui/test/**/*.ts"],
    rules: { "@typescript-eslint/no-explicit-any": "off" },
  },
);
