# Skills

Pi supports skills — markdown files that extend the agent's capabilities by
providing instructions and examples for specific tasks. Each skill is a directory
containing a `SKILL.md` file with a YAML frontmatter (`name`, `description`) and
the skill's instructions.

OpenCrow ships with a `web` skill (for browsing with curl/lynx) and passes it to
pi by default.

## NixOS module

The NixOS module provides a declarative `skills` option — an attrset mapping
skill names to directories:

```nix
services.opencrow.skills = {
  web = "${pkgs.opencrow}/share/opencrow/skills/web";  # included by default
  kagi-search = "${mics-skills}/skills/kagi-search";
  my-custom-skill = ./skills/my-custom-skill;
};
```

All entries are assembled into a single directory via `linkFarm` and passed to
pi through `OPENCROW_PI_SKILLS_DIR`. The attrset is mergeable, so skills can be
added from multiple NixOS module files.

## Environment variables

When not using the NixOS module, configure skills via environment variables:

```
OPENCROW_PI_SKILLS=/path/to/skill1,/path/to/skill2
OPENCROW_PI_SKILLS_DIR=/path/to/skills-directory
```

`OPENCROW_PI_SKILLS` is a comma-separated list of individual skill directories.
`OPENCROW_PI_SKILLS_DIR` points to a directory whose subdirectories are scanned
for `SKILL.md` files. Both can be used together.

## Writing a skill

Create a directory with a `SKILL.md`:

```markdown
---
name: my-skill
description: What this skill does and when to use it
---

Instructions for the agent on how to use this skill...
```
