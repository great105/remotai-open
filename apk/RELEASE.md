# Android для своего сервера

Нужны Node.js 22+, JDK 21, Android SDK и собственный ключ для release-сборки.
Сначала установите зависимости командой `npm ci` из корня репозитория.

В PowerShell из корня:

```powershell
$env:VITE_RELAY_BASE = "https://remotai.example.com"
$env:VITE_BASE = "/"
npm run build:apk
cd apk
npx cap sync android
cd android
.\gradlew.bat assembleDebug
```

На Linux задайте те же переменные окружения и используйте `./gradlew assembleDebug`.
Результат: `apk/android/app/build/outputs/apk/debug/app-debug.apk`.

Для release создайте свой keystore и заполните `apk/android/keystore.properties`
по примеру `keystore.properties.example`. Выполните `assembleRelease` для APK
или `bundleRelease` для AAB. Не добавляйте ключ и пароли в Git.
Для последующих обновлений своего приложения сохраняйте один ключ подписи.

При распространении форка используйте свой `applicationId` и название.
Версии задаются в `apk/android/app/build.gradle`. Release без своего ключа
не подписывается; официальный ключ Remotai в репозиторий не входит.
