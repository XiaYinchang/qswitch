# launchd

This file is a **placeholder**. Do not copy it as-is: `HOME` and `QSWITCHD` are not expanded by launchd.

`qswitch init` writes a filled plist to `~/.qswitch/com.qswitch.plist`. Install with:

```
cp ~/.qswitch/com.qswitch.plist ~/Library/LaunchAgents/com.qswitch.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.qswitch.plist
```

KeepAlive defaults to false until Keychain prompts are confirmed.
