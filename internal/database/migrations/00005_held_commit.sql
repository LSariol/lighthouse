-- Going back by hand on a project that deploys its branch.
--
-- projects.held_sha: the branch's newest commit when someone went back to an
--                    older one (`rollback`, or `deploy <name> <commit>`).
--                    Checks don't deploy it again; the next new commit, or
--                    `deploy <name>`, does.

-- +goose Up
ALTER TABLE lighthouse.projects
    ADD COLUMN held_sha text;
