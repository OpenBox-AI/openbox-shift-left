# Deploying the Codex requirements via MDM (macOS)

Codex reads managed configuration from the `com.openai.codex` preference domain
as base64-encoded TOML, which is how Jamf, Fleet and Kandji deliver it.

Generate the payload value:

```bash
base64 -i requirements.toml | tr -d '\n'
```

Then deliver a configuration profile for domain `com.openai.codex` with that
string as the managed-requirements value, per your MDM's custom-settings
workflow.

On Linux, deploy the file itself to `/etc/codex/requirements.toml`, root-owned
and mode 0644; readable by the developer, writable only by root. A requirements
file the developer can edit provides no mandate.

Where your org has **cloud-managed requirement bundles**, prefer them: they are
delivered by the provider rather than by your fleet tooling, so a machine that
misses an MDM push is not silently unmanaged.
