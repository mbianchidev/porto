!include "LogicLib.nsh"
!include "MUI2.nsh"
!include "nsDialogs.nsh"

!ifndef BUILD_UNINSTALLER
  Var DesktopShortcutCheckbox
  Var DesktopShortcutRequested

  !macro customInit
    StrCpy $DesktopShortcutRequested "false"
  !macroend

  !macro customPageAfterChangeDir
    Function desktopShortcutPageCreate
      ${If} ${isUpdated}
        Abort
      ${EndIf}

      nsDialogs::Create 1018
      Pop $0
      ${If} $0 == error
        Abort
      ${EndIf}

      !insertmacro MUI_HEADER_TEXT "Additional options" "Choose whether Porto creates a desktop shortcut."
      ${NSD_CreateCheckbox} 0 0 100% 12u "Create a desktop shortcut"
      Pop $DesktopShortcutCheckbox
      ${NSD_SetState} $DesktopShortcutCheckbox ${BST_UNCHECKED}

      nsDialogs::Show
    FunctionEnd

    Function desktopShortcutPageLeave
      ${NSD_GetState} $DesktopShortcutCheckbox $0
      ${If} $0 == ${BST_CHECKED}
        StrCpy $DesktopShortcutRequested "true"
      ${Else}
        StrCpy $DesktopShortcutRequested "false"
      ${EndIf}
    FunctionEnd

    Page custom desktopShortcutPageCreate desktopShortcutPageLeave
  !macroend

  !macro customInstall
    ${If} $DesktopShortcutRequested == "true"
      CreateShortCut "$newDesktopLink" "$appExe" "" "$appExe" 0 "" "" "${APP_DESCRIPTION}"
      ClearErrors
      WinShell::SetLnkAUMI "$newDesktopLink" "${APP_ID}"
      System::Call 'shell32::SHChangeNotify(i, i, i, i) v (0x08000000, 0, 0, 0)'
    ${EndIf}
  !macroend
!endif

!macro customUnInstall
  ${IfNot} ${isKeepShortcuts}
    WinShell::UninstShortcut "$oldDesktopLink"
    Delete "$oldDesktopLink"
  ${EndIf}
!macroend
