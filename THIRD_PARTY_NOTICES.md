# Third-party notices

## WinDivert

- Project: https://reqrypt.org/windivert.html
- Version bundled: 2.2.2-A (x64: `WinDivert64.dll`, `WinDivert64.sys`)
- License: LGPL-3.0-or-later OR GPL-2.0-or-later (dual licensed)
- Copyright: Basil (reqrypt.org) and contributors.

The x64 DLL and driver are embedded unmodified in `Civ6RelayClient.exe`
(`internal/win/assets/`) and extracted to
`%ProgramData%\Civ6Relay\windivert-2.2.2\` at runtime, where they are loaded
dynamically. The full license text is bundled as
`internal/win/assets/WinDivert-LICENSE.txt` and is also written next to the
extracted binaries.

Source code for WinDivert is available from the upstream project. Users may
replace the extracted DLL/SYS with their own builds of the same version.

## Build-time only

`github.com/akavel/rsrc` (MIT) was used once to generate
`cmd/client/rsrc_windows_amd64.syso` from `cmd/client/app.manifest`. The tool is
not linked into the client and is not required to build it.
