"""Build desktop installers from an already built, versioned Remotai binary."""
from pathlib import Path
import argparse
import hashlib
import json
import os
import platform
import plistlib
import re
import shlex
import shutil
import struct
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def run(*args):
    try:
        return subprocess.check_output([str(a) for a in args], text=True, stderr=subprocess.STDOUT)
    except subprocess.CalledProcessError as exc:
        print(exc.output)
        raise


def copy(source, target, mode=0o644):
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, target)
    target.chmod(mode)


def write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(value, encoding='utf8')


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def build_mac(args, work):
    if platform.system() != 'Darwin':
        raise RuntimeError('macOS packaging requires a macOS runner')
    app = work / 'image/Remotai.app'
    binary = app / 'Contents/MacOS/Remotai'
    binary.parent.mkdir(parents=True)
    run('lipo', '-create', args.binary, args.intel_binary, '-output', binary)
    binary.chmod(0o755)
    assert set(run('lipo', '-archs', binary).split()) == {'arm64', 'x86_64'}
    assert f'Remotai v{args.version} ' in run(binary, 'version')
    native = app / 'Contents/MacOS/RemotaiWindow'
    windows = []
    for arch in ['arm64', 'x86_64']:
        target = work / f'window-{arch}'
        run('xcrun', 'swiftc', '-O', '-swift-version', '5', '-target', f'{arch}-apple-macos12.0',
            '-framework', 'AppKit', '-framework', 'WebKit', ROOT / 'desktop/macos/main.swift', '-o', target)
        windows.append(target)
    run('lipo', '-create', *windows, '-output', native)
    native.chmod(0o755)
    assert set(run('lipo', '-archs', native).split()) == {'arm64', 'x86_64'}
    (app / 'Contents/Info.plist').write_bytes(plistlib.dumps({
        'CFBundleIdentifier': 'ru.remotai.desktop',
        'CFBundleName': 'Remotai', 'CFBundleDisplayName': 'Remotai',
        'CFBundleExecutable': 'RemotaiWindow', 'CFBundlePackageType': 'APPL',
        'CFBundleVersion': args.version, 'CFBundleShortVersionString': args.version,
        'CFBundleIconFile': 'remotai.icns', 'LSMinimumSystemVersion': '12.0',
        'LSUIElement': False, 'NSHighResolutionCapable': True,
        'NSAppTransportSecurity': {'NSAllowsLocalNetworking': True},
        'NSMicrophoneUsageDescription': 'Remotai использует микрофон для голосового ввода по вашему нажатию.',
        'NSCameraUsageDescription': 'Remotai использует камеру для сканирования кода подключения.',
    }))
    icon = (ROOT / 'internal/desktopentry/icon.png').read_bytes()
    resources = app / 'Contents/Resources'
    resources.mkdir()
    (resources / 'remotai.icns').write_bytes(b'icns' + struct.pack('>I', len(icon) + 16) + b'ic09' + struct.pack('>I', len(icon) + 8) + icon)
    (work / 'image/Applications').symlink_to('/Applications')
    write(work / 'image/Как установить.txt',
          'Перетащите Remotai в папку Applications (Программы), затем откройте Remotai.\n'
          'Remotai откроется в собственном окне. Значок приложения останется в Dock.\n'
          'Закрытие окна не останавливает терминалы и доступ с телефона.\n'
          'Состояние подписи и помощь: https://remotai.ru/app/#/start?task=host&os=macos\n')
    identity = args.signing_identity or '-'
    sign = ['codesign', '--force', '--sign', identity, '--options', 'runtime']
    sign += ['--timestamp'] if args.signing_identity else ['--timestamp=none']
    run(*sign, binary)
    run(*sign, native)
    run(*sign, app)
    run('codesign', '--verify', '--deep', '--strict', '--verbose=2', app)
    package = args.out / 'remotai-macos.dmg'
    run('hdiutil', 'create', '-volname', 'Remotai', '-srcfolder', work / 'image', '-format', 'UDZO', '-ov', package)
    if args.signing_identity:
        run('codesign', '--force', '--sign', identity, '--timestamp', package)
    notarized = False
    if args.notary_profile:
        if not args.signing_identity:
            raise RuntimeError('Notarization requires Developer ID signing')
        result = json.loads(run('xcrun', 'notarytool', 'submit', package, '--keychain-profile', args.notary_profile, '--wait', '--output-format', 'json'))
        if result.get('status') != 'Accepted':
            raise RuntimeError('Apple did not accept this package for notarization')
        run('xcrun', 'stapler', 'staple', package)
        run('xcrun', 'stapler', 'validate', package)
        notarized = True
    return [package], {'developer_id_signed': bool(args.signing_identity), 'notarized': notarized,
                       'native_window': True, 'architectures': ['arm64', 'amd64']}


def build_linux(args, work):
    if platform.system() != 'Linux':
        raise RuntimeError('Linux packaging requires a Linux runner')
    assert f'Remotai v{args.version} ' in run(args.binary, 'version')
    payload = work / 'root'
    copy(args.binary, payload / 'usr/lib/remotai/remotai', 0o755)
    copy(ROOT / 'internal/desktopentry/icon.png', payload / 'usr/share/icons/hicolor/512x512/apps/remotai.png')
    write(payload / 'usr/share/applications/ru.remotai.desktop', '''[Desktop Entry]
Type=Application
Name=Remotai
Comment=Access your AI agents
Comment[ru]=Удалённый доступ к вашим ИИ-агентам
Exec=/usr/lib/remotai/remotai --desktop
TryExec=/usr/lib/remotai/remotai
Icon=remotai
Terminal=false
StartupNotify=false
Categories=Development;Network;
''')
    write(payload / 'usr/share/metainfo/ru.remotai.metainfo.xml', '''<?xml version="1.0" encoding="UTF-8"?>
<component type="desktop-application">
  <id>ru.remotai</id><name>Remotai</name>
  <summary>Access your AI agents from another device</summary>
  <metadata_license>CC0-1.0</metadata_license><project_license>LicenseRef-proprietary</project_license>
  <description><p>Open your projects, files and AI agent terminals from your phone or another computer.</p></description>
  <launchable type="desktop-id">ru.remotai.desktop</launchable>
  <url type="homepage">https://remotai.ru</url>
</component>
''')
    run('desktop-file-validate', payload / 'usr/share/applications/ru.remotai.desktop')
    write(payload / 'DEBIAN/control', f'''Package: remotai
Version: {args.version}
Section: net
Priority: optional
Architecture: {args.arch}
Maintainer: Remotai
Depends: xdg-utils, zenity
Homepage: https://remotai.ru
Description: Access your AI agents from another device
 Open Remotai from your applications menu to connect this computer.
''')
    deb = args.out / f'remotai-linux-{args.arch}.deb'
    run('dpkg-deb', '--build', '--root-owner-group', payload, deb)
    rpm_root = work / 'rpm'
    rpm_arch = {'amd64': 'x86_64', 'arm64': 'aarch64'}[args.arch]
    spec = rpm_root / 'SPECS/remotai.spec'
    write(spec, f'''Name: remotai
Version: {args.version}
Release: 1
Summary: Access your AI agents from another device
License: LicenseRef-proprietary
URL: https://remotai.ru
BuildArch: {rpm_arch}
Requires: xdg-utils, zenity
AutoReqProv: no
%global debug_package %{{nil}}
%global __strip /bin/true
%description
Open Remotai from your applications menu to connect this computer.
%install
mkdir -p %{{buildroot}}
cp -a {shlex.quote(str(payload / 'usr'))} %{{buildroot}}/
%files
/usr/lib/remotai/remotai
/usr/share/applications/ru.remotai.desktop
/usr/share/icons/hicolor/512x512/apps/remotai.png
/usr/share/metainfo/ru.remotai.metainfo.xml
''')
    run('rpmbuild', '-bb', '--define', f'_topdir {rpm_root}', '--target', rpm_arch, spec)
    built = list((rpm_root / 'RPMS').rglob('*.rpm'))
    assert len(built) == 1
    rpm = args.out / f'remotai-linux-{args.arch}.rpm'
    copy(built[0], rpm)
    return [deb, rpm], {'architectures': [args.arch]}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--intel-binary', type=Path)
    parser.add_argument('--version', required=True)
    parser.add_argument('--source', required=True)
    parser.add_argument('--arch', choices=['amd64', 'arm64'], required=True)
    parser.add_argument('--platform', choices=['macos', 'linux'], required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--signing-identity', default='')
    parser.add_argument('--notary-profile', default='')
    args = parser.parse_args()
    if not re.fullmatch(r'\d+\.\d+\.\d+', args.version) or not re.fullmatch(r'[0-9a-f]{40}', args.source):
        parser.error('Expected a release version and full source commit')
    args.out = args.out.resolve()
    args.binary = args.binary.resolve()
    args.out.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='remotai-package-') as scratch:
        packages, properties = (build_mac if args.platform == 'macos' else build_linux)(args, Path(scratch))
    proof = {'version': args.version, 'source': args.source, 'platform': args.platform, **properties,
             'artifacts': [{'filename': path.name, 'size': path.stat().st_size, 'sha256': digest(path)} for path in packages]}
    (args.out / 'packages.json').write_text(json.dumps(proof, indent=2), encoding='utf8')
    print(json.dumps(proof, indent=2))


if __name__ == '__main__':
    main()
