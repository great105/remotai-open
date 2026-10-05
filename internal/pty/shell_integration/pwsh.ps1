# Remotai: разметка команд (OSC 133) для Windows PowerShell 5.1 и pwsh 7. ST-10, T-39.
#
# Передаётся НЕ файлом, а через -NoExit -EncodedCommand: политика выполнения
# Restricted (умолчание на клиентской Windows) запрещает .ps1, а команду из
# командной строки — нет (живая проверка 13.09.2026: powershell 5.1 и pwsh
# 7.6.6 с -ExecutionPolicy Restricted — .ps1 «running scripts is disabled»,
# -EncodedCommand выполнился). Профиль пользователя грузится как обычно (без
# -NoProfile) и ДО этого кода, поэтому здесь оборачивается уже итоговая
# функция prompt — хоть стандартная, хоть oh-my-posh или starship.
#
# Полнострочные комментарии вырезаются перед кодированием (shell_integration.go):
# командная строка Windows ограничена 32K, а base64 от UTF-16 раздувает код
# вчетверо.
if (-not $global:__RemotaiSI) {
    $global:__RemotaiSI = $true
    $global:__RemotaiSIPrompt = $function:prompt
    $global:__RemotaiSILastId = $null
    function global:prompt {
        # $? и $LASTEXITCODE — первым делом, пока их не перетёрла наша же работа.
        $remotaiOk = $global:?
        $remotaiLec = $global:LASTEXITCODE
        $remotaiE = [char]0x1b
        $remotaiBel = [char]7
        $remotaiOut = ''
        # D — только если с прошлого приглашения выполнилась команда: пустой
        # Enter и Ctrl+C в историю не попадают, и блока «без команды» не будет.
        $remotaiH = Get-History -Count 1
        $remotaiId = 0
        if ($remotaiH) { $remotaiId = $remotaiH.Id }
        if ($null -ne $global:__RemotaiSILastId -and $remotaiId -ne $global:__RemotaiSILastId) {
            $remotaiCode = 0
            if (-not $remotaiOk) {
                $remotaiCode = 1
                if ($remotaiLec -is [int] -and $remotaiLec -ne 0) { $remotaiCode = $remotaiLec }
            }
            $remotaiOut += "$remotaiE]133;D;$remotaiCode$remotaiBel"
        }
        $global:__RemotaiSILastId = $remotaiId
        $remotaiOut += "$remotaiE]133;A$remotaiBel"
        # D и A — сразу на экран, а не в возвращаемую строку. prompt из профиля,
        # который печатает через Write-Host (старый posh-git, самодельные),
        # выводит свой текст ДО того, как хост напечатает возвращённую строку:
        # видимое приглашение легло бы перед D (клиент счёл бы его выводом
        # прошлой команды) и мимо A…B (ревью B2, живая проверка). PSReadLine
        # (InvokePrompt, Ctrl+L) ставит курсор на место ДО вызова prompt, так что
        # A остаётся в начале приглашения. Хост без консоли не пишет — тогда,
        # как раньше, строкой.
        try { $Host.UI.Write($remotaiOut); $remotaiOut = '' } catch {}
        $global:LASTEXITCODE = $remotaiLec
        $remotaiUser = 'PS> '
        # Пользовательский prompt (oh-my-posh, starship) красит себя по $? —
        # возвращаем ему прежнее значение, как это делает VS Code. Write-Error
        # обязан стоять ВПЛОТНУЮ к вызову: любое присваивание между ними снова
        # делает $? истинным (живая проверка 13.09.2026: prompt видел True
        # после упавшей команды, пока между ними было присваивание).
        try {
            if ($global:__RemotaiSIPrompt) {
                if (-not $remotaiOk) {
                    Write-Error 'remotai' -ErrorAction Ignore; $remotaiUser = -join @($global:__RemotaiSIPrompt.Invoke())
                } else {
                    $remotaiUser = -join @($global:__RemotaiSIPrompt.Invoke())
                }
            }
        } catch {}
        $global:LASTEXITCODE = $remotaiLec
        $remotaiOut + $remotaiUser + "$remotaiE]133;B$remotaiBel"
    }
}
