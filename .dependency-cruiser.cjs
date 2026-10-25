// Dependency rules for the browser bundle; docs/architecture.md explains the
// layering. Models are pure: no component, no host call.
const models = "^ui/src/([a-z-]+-model|detail|references|review-store)\\.ts$";

module.exports = {
  forbidden: [
    { name: "no-circular", severity: "error", from: {}, to: { circular: true } },
    {
      name: "models-stay-pure",
      comment: "A model must not import a component or the entry point.",
      severity: "error",
      from: { path: models },
      to: { path: "^ui/src/", pathNot: [models, "^ui/src/host-contract\\.ts$"] },
    },
    {
      name: "entry-is-a-leaf",
      comment: "Nothing imports bundle.ts: it is what the host loads.",
      severity: "error",
      from: { pathNot: "^ui/test/" },
      to: { path: "^ui/src/bundle\\.ts$" },
    },
    {
      name: "no-orphans",
      severity: "error",
      from: { orphan: true, path: "^ui/src/", pathNot: ["\\.d\\.ts$", "^ui/src/host-contract\\.ts$"] },
      to: {},
    },
    {
      name: "tests-not-from-src",
      severity: "error",
      from: { path: "^ui/src/" },
      to: { path: "^ui/test/" },
    },
  ],
  options: {
    doNotFollow: { path: "node_modules" },
    tsPreCompilationDeps: true,
    exclude: { path: "ui/bundle\\.js$" },
  },
};
