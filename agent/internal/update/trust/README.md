# Update trust

`snugbox self-update` trusts what is in this folder and nothing else. The release build embeds it.

- `root.json` is the first root metadata of the update repository, signed by the root keys on the owner's two hardware keys (blueprint section 6.3). It is public: it holds public keys and signatures only.
- `repository.json` says where the repository serves its metadata and its files:

```json
{"metadata_url": "https://example.org/metadata/", "targets_url": "https://example.org/targets/"}
```

Until both files are here, a build's `snugbox self-update` says it has no update root and that Snugbox is updated by running the install script again. [docs/maintaining.md](../../../../docs/maintaining.md) says how the owner makes the root and publishes a release to the repository.
