---
repository: dsmolchanov/nerve-oss
status: draft
---

# Publish the frozen v0.0.25 runtime without rebuilding

The protected Cloud release set `36104390910:7033bc5e29c04202f3990bc67f122264472d61b384e3864ab2c550cae9ac0af2` runs the OSS target image built by Docker Publish candidate run `36104220018`. Its source is `7b95eae7ca89c3b99decd989d04d18a40fa2b9a1`, while OSS main has advanced. The existing tag-push job builds a fresh image, which would not be the reviewed production bytes.

Add a one-use, protected workflow under `.github/workflows/` on main that authenticates the successful candidate run, exact source, manifest, signature and both immutable digests. Before mutation it proves that the version is unused as a tag, GitHub Release and in both GHCR repositories, and runs the self-host lifecycle smoke against the candidate digest. The v0.0.25 successor service deliberately rejects startup migration and requires Core31/Cloud3 already present. In the smoke's isolated database, use the immutable published v0.0.20 predecessor migrator for Core29/Cloud3, then the candidate migrator for Core31, then test the candidate service's clean start, pairing recreation and backup/restore. No production database is touched. The workflow then copies that exact image index to both published GHCR tags, verifies the resulting tag digests, creates an annotated tag at the candidate source through the workflow `GITHUB_TOKEN`, and publishes a Release with assets generated from the exact candidate source and manifest. The token-created tag does not trigger the older tag-push workflow. The workflow must report partial publication and never overwrite or automatically retry it.

Acceptance: a reviewer can compare all pinned values with the signed Cloud release set and candidate artifact; the workflow has a protected environment, branch and confirmation guard; the published tag resolves to the exact source and an annotated tag object; both GHCR references resolve to the frozen index digest; Release assets match the candidate manifest and source; the self-host smoke passes on the immutable digest. Do not dispatch the workflow until required CI and head-bound review pass.
