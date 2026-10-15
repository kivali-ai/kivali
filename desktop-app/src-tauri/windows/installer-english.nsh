; Kivali's English strings for Tauri's NSIS installer and uninstaller
; (tauri.windows.conf.json, bundle.windows.nsis.customLanguageFiles),
; in place of Tauri's own English.nsh: the same LangString names, worded
; for Kivali. The upgrade page recommends installing over the version
; already there, which is what the in-app update does, and says the
; teams are kept either way; the uninstaller's checkbox says what it
; deletes: the WebView2 profile under %LOCALAPPDATA%\<identifier>
; (cookies, so sign-ins to teams). The teams list (%LOCALAPPDATA%\Kivali)
; and the teams' disks (C:\ProgramData\Kivali\broker\vm) are never
; touched by the uninstaller.
;
; Tauri includes this file after its pages and variables, so it also
; holds KivaliGUIInit, which installer-hooks.nsh names as MUI's GUI-init
; function (the hooks file is included before the variables exist).

LangString addOrReinstall ${LANG_ENGLISH} "Add or reinstall components"
LangString alreadyInstalled ${LANG_ENGLISH} "Already installed"
LangString alreadyInstalledLong ${LANG_ENGLISH} "${PRODUCTNAME} ${VERSION} is already installed. Select what you want to do and click Next to continue."
LangString appRunning ${LANG_ENGLISH} "${PRODUCTNAME} is running. Close it, then try again."
LangString appRunningOkKill ${LANG_ENGLISH} "${PRODUCTNAME} is running.$\nClick OK to close it."
LangString chooseMaintenanceOption ${LANG_ENGLISH} "Choose what to do."
LangString choowHowToInstall ${LANG_ENGLISH} "Choose how to install ${PRODUCTNAME}."
LangString createDesktop ${LANG_ENGLISH} "Create desktop shortcut"
LangString dontUninstall ${LANG_ENGLISH} "Install over the current version (recommended)"
LangString dontUninstallDowngrade ${LANG_ENGLISH} "Install over the current version (not available for an older version)"
LangString failedToKillApp ${LANG_ENGLISH} "Could not close ${PRODUCTNAME}. Close it, then try again."
LangString installingWebview2 ${LANG_ENGLISH} "Installing WebView2..."
LangString newerVersionInstalled ${LANG_ENGLISH} "A newer version of ${PRODUCTNAME} is installed. To install this older version, uninstall the current one first. Your teams are kept either way. Select what you want to do and click Next to continue."
LangString older ${LANG_ENGLISH} "older"
LangString olderOrUnknownVersionInstalled ${LANG_ENGLISH} "An $R4 version of ${PRODUCTNAME} is installed. Installing over it keeps your teams, settings and sign-ins. Select what you want to do and click Next to continue."
LangString silentDowngrades ${LANG_ENGLISH} "This installer cannot install an older version silently. Run it without /S instead.$\n"
LangString unableToUninstall ${LANG_ENGLISH} "Could not uninstall the current version."
LangString uninstallApp ${LANG_ENGLISH} "Uninstall ${PRODUCTNAME}"
LangString uninstallBeforeInstalling ${LANG_ENGLISH} "Uninstall the current version first"
LangString unknown ${LANG_ENGLISH} "unknown"
LangString webview2AbortError ${LANG_ENGLISH} "Could not install WebView2, which ${PRODUCTNAME} needs to run. Restart the installer to try again."
LangString webview2DownloadError ${LANG_ENGLISH} "Could not download WebView2: $0"
LangString webview2DownloadSuccess ${LANG_ENGLISH} "Downloaded the WebView2 installer"
LangString webview2Downloading ${LANG_ENGLISH} "Downloading the WebView2 installer..."
LangString webview2InstallError ${LANG_ENGLISH} "Could not install WebView2 (exit code $1)"
LangString webview2InstallSuccess ${LANG_ENGLISH} "Installed WebView2"
LangString deleteAppData ${LANG_ENGLISH} "Also delete browser data and sign-ins (teams are kept)"

; Selects "Install over the current version" when an older version is
; installed, rather than Tauri's default of uninstalling it first. A full
; uninstall removes the broker service and its confirmation page offers
; to delete app data, neither of which an upgrade wants. PageReinstall checks its second button when
; ReinstallPageCheck is 2; for the same version that button is the
; uninstall, so nothing changes then, nor for an older installer.
Function KivaliGUIInit
  Push $0
  ReadRegStr $0 SHCTX "${UNINSTKEY}" "DisplayVersion"
  ${If} $0 != ""
    nsis_tauri_utils::SemverCompare "${VERSION}" $0
    Pop $0
    ${If} $0 = 1
      StrCpy $ReinstallPageCheck 2
    ${EndIf}
  ${EndIf}
  Pop $0
FunctionEnd
