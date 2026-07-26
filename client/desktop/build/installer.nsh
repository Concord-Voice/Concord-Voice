; Concord Voice NSIS customization — issue #2402.
;
; Forces the install directory to $LOCALAPPDATA\ConcordVoice\app so that the
; electron-updater download cache ($LOCALAPPDATA\ConcordVoice\pending) is a
; SIBLING of the install root rather than a child of it.
;
; Squirrel.Windows recursive-deleted its own install root before writing, which
; meant deleting the very installer executing from pending\ — Windows locks a
; running image, the delete threw UnauthorizedAccessException, and the installer
; died. NSIS scopes uninstallOldVersion to $INSTDIR and cannot reach a sibling.
;
; Do NOT set this to $LOCALAPPDATA\ConcordVoice: that re-nests the cache inside
; the install root and re-opens the same class of failure from a new installer.
;
; This must be baked into the artifact rather than passed as /D= at runtime,
; because the migration hop is executed by the OLD client, which sets no
; installDirectory and therefore passes no /D=.
;
; See [internal]specs/2026-07-25-2402-windows-nsis-migration-design.md §3.2.

!macro customInit
  StrCpy $INSTDIR "$LOCALAPPDATA\ConcordVoice\app"
!macroend
