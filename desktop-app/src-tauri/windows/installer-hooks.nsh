; Kivali's hooks into Tauri's NSIS installer (tauri.windows.conf.json,
; bundle.windows.nsis.installerHooks). The install is per machine, so the
; installer and the uninstaller run elevated, and they register and remove
; the broker: kivali-supervisor's service (its own virtual account,
; NT SERVICE\kivali-broker, a Hyper-V administrator), which manages the
; teams' Hyper-V VMs (docs/developers/supervisor.md, "The broker";
; docs/developers/desktop-app.md, "Signing and release"). A broker step that fails is
; written to the details and explained; it never stops the install or the
; uninstall.
;
; System tools are named by their full path: CreateProcess looks in the
; installer's own folder first, and an elevated installer must not run a
; net.exe planted beside it in Downloads.
;
; Kivali running is asked about before the broker is touched, so a Cancel
; there leaves the service as it was (the template asks again right after
; and finds nothing running).

!define KIVALI_BROKER "kivali-broker"

; $0 is 0 when the broker service is registered (sc answers 1060 when not).
!macro KivaliBrokerRegistered
  nsExec::Exec '"$SYSDIR\sc.exe" query ${KIVALI_BROKER}'
  Pop $0
!macroend

; Stops the broker if it is registered. net waits for the service to stop,
; which releases kivali-supervisor.exe; not started is not an error.
!macro KivaliBrokerStop
  !insertmacro KivaliBrokerRegistered
  ${If} $0 == 0
    DetailPrint "Stopping the Kivali broker service"
    nsExec::ExecToLog '"$SYSDIR\net.exe" stop ${KIVALI_BROKER}'
    Pop $0
  ${EndIf}
!macroend

; Before the files are copied: an upgrade replaces kivali-supervisor.exe,
; which the running broker holds open.
!macro NSIS_HOOK_PREINSTALL
  !insertmacro CheckIfAppIsRunning "$INSTDIR\${MAINBINARYNAME}.exe" "${PRODUCTNAME}"
  !insertmacro KivaliBrokerStop
!macroend

; After the files: register the service and start it. `broker install`
; on an upgrade finds the service registered at this same path and
; refreshes it (the binary it runs, its privileges, the entries newer
; versions add) before starting it. Then say so when Hyper-V is not
; enabled, which only an administrator can do, with a restart; the
; installer does not.
!macro NSIS_HOOK_POSTINSTALL
  DetailPrint "Registering the Kivali broker service"
  nsExec::ExecToLog '"$INSTDIR\kivali-supervisor.exe" broker install'
  Pop $0
  ${If} $0 != 0
    ; A refresh that failed part way leaves the service registered;
    ; start what is there (already started is exit 2 too, NET HELPMSG
    ; 2182; the service also starts with Windows).
    !insertmacro KivaliBrokerRegistered
    ${If} $0 == 0
      nsExec::ExecToLog '"$SYSDIR\net.exe" start ${KIVALI_BROKER}'
      Pop $0
    ${EndIf}
  ${EndIf}
  ${If} $0 != 0
    DetailPrint "Kivali could not register its broker service (${KIVALI_BROKER})"
    ${If} $PassiveMode != 1
      MessageBox MB_ICONEXCLAMATION|MB_OK "Kivali could not register its broker service (${KIVALI_BROKER}), so teams on this computer cannot start yet.$\r$\n$\r$\nRegister it from an administrator's terminal:$\r$\n$\"$INSTDIR\kivali-supervisor.exe$\" broker install" /SD IDOK
    ${EndIf}
  ${EndIf}
  ; vmms, Hyper-V's management service, exists once the role is enabled.
  nsExec::Exec '"$SYSDIR\sc.exe" query vmms'
  Pop $0
  ${If} $0 != 0
    DetailPrint "Hyper-V is not enabled on this computer"
    ${If} $PassiveMode != 1
      MessageBox MB_ICONINFORMATION|MB_OK "Kivali runs each team on this computer in a Hyper-V virtual machine, and Hyper-V is not enabled here.$\r$\n$\r$\nAn administrator can enable it in Turn Windows features on or off (Hyper-V), then restart the computer. Teams on other computers work without it." /SD IDOK
    ${EndIf}
  ${EndIf}
!macroend

; Before the files are removed: stop the broker (which releases
; kivali-supervisor.exe) and remove the service. An update's uninstall
; keeps the service for the version being installed.
!macro NSIS_HOOK_PREUNINSTALL
  !insertmacro CheckIfAppIsRunning "$INSTDIR\${MAINBINARYNAME}.exe" "${PRODUCTNAME}"
  !insertmacro KivaliBrokerStop
  !insertmacro KivaliBrokerRegistered
  ${If} $0 == 0
  ${AndIf} $UpdateMode <> 1
    DetailPrint "Removing the Kivali broker service"
    nsExec::ExecToLog '"$INSTDIR\kivali-supervisor.exe" broker uninstall'
    Pop $0
    ${If} $0 != 0
      DetailPrint "Kivali could not fully remove its broker service (${KIVALI_BROKER}); see the details above"
      ${If} $PassiveMode != 1
        MessageBox MB_ICONEXCLAMATION|MB_OK "Kivali could not fully remove its broker service (${KIVALI_BROKER}); the details above say which step failed. The uninstall goes on. Afterwards, from an administrator's terminal: if $\"sc.exe query ${KIVALI_BROKER}$\" still lists the service, $\"sc.exe delete ${KIVALI_BROKER}$\"; and a C:\ProgramData\Kivali\broker left behind holds the Hyper-V files of teams whose VMs still exist (Remove-VM in PowerShell), which can then be deleted." /SD IDOK
      ${EndIf}
    ${EndIf}
  ${EndIf}
!macroend
