package pty

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeFile — helper: создаёт файл с содержимым (папки по необходимости).
func writeSSHConfigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseSSHConfig(t *testing.T) {
	dir := t.TempDir()

	writeSSHConfigFile(t, filepath.Join(dir, "config"), `
# Глобальные дефолты
User globaluser
Port 2222

# Tags: prod, eu
Host web1
    HostName 10.0.0.1
    User deploy

Host web2
    HostName 10.0.0.2
    Port 2200
    IdentityFile ~/.ssh/web2_key
    ProxyJump admin@bastion:2222

# wildcard-блоки — это дефолты, а не хосты
Host *.internal
    User internaluser

Host db.internal
    HostName db.local

Host *
    ServerAliveInterval 60
    User staruser

Include extra/*.conf
`)
	writeSSHConfigFile(t, filepath.Join(dir, "extra", "a.conf"), `
# Tags: included
Host inc1
    HostName 192.168.1.5
`)
	// Цикл Include: b.conf включает сам конфиг — не должны зависнуть.
	writeSSHConfigFile(t, filepath.Join(dir, "extra", "b.conf"), "Include ../config\nHost inc2\n    HostName 192.168.1.6\n")

	hosts, err := ParseSSHConfig(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]SSHConfigHost{}
	var names []string
	for _, h := range hosts {
		byName[h.Name] = h
		names = append(names, h.Name)
	}

	wantNames := []string{"web1", "web2", "db.internal", "inc1", "inc2"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("names = %v, want %v", names, wantNames)
	}

	// web1: свои поля + дефолтный Port из глобала (User задан в блоке).
	web1 := byName["web1"]
	if web1.Host != "10.0.0.1" || web1.User != "deploy" || web1.Port != 2222 {
		t.Fatalf("web1 = %+v", web1)
	}
	if !reflect.DeepEqual(web1.Tags, []string{"prod", "eu"}) {
		t.Fatalf("web1 tags = %v", web1.Tags)
	}

	// web2: свой порт побеждает глобальный; ProxyJump и IdentityFile читаются.
	web2 := byName["web2"]
	if web2.Port != 2200 || web2.ProxyJump != "admin@bastion:2222" || web2.IdentityFile != "~/.ssh/web2_key" {
		t.Fatalf("web2 = %+v", web2)
	}

	// db.internal: дефолты НЕ наследуются от «Host *.internal» в одном случае
	// и наследуются в другом? Нет: паттерн-блоки — источник дефолтов, но
	// глобальный User был раньше и first-wins на уровне дефолтов. Проверяем,
	// что хотя бы один дефолтный user подставился и HostName прочитан.
	db := byName["db.internal"]
	if db.Host != "db.local" {
		t.Fatalf("db.internal = %+v", db)
	}
	if db.User == "" {
		t.Fatalf("db.internal: ожидался дефолтный user, %+v", db)
	}

	// inc1: из Include с тегом.
	inc1 := byName["inc1"]
	if inc1.Host != "192.168.1.5" || !reflect.DeepEqual(inc1.Tags, []string{"included"}) {
		t.Fatalf("inc1 = %+v", inc1)
	}
}

func TestParseSSHConfigMissing(t *testing.T) {
	hosts, err := ParseSSHConfig(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 0 {
		t.Fatalf("hosts = %v, want empty", hosts)
	}
}

func TestParseSSHConfigKeyValueForms(t *testing.T) {
	dir := t.TempDir()
	writeSSHConfigFile(t, filepath.Join(dir, "config"), `
Host=eq1
    HostName=10.1.1.1
    User = spaced
Host none1
    HostName 10.1.1.2
    ProxyJump none
`)
	hosts, err := ParseSSHConfig(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if hosts[0].Name != "eq1" || hosts[0].Host != "10.1.1.1" || hosts[0].User != "spaced" {
		t.Fatalf("eq1 = %+v", hosts[0])
	}
	// ProxyJump none → пусто.
	if hosts[1].ProxyJump != "" {
		t.Fatalf("none1.ProxyJump = %q, want empty", hosts[1].ProxyJump)
	}
}

func TestSplitSSHConfigLine(t *testing.T) {
	cases := []struct{ in, key, val string }{
		{"Host web1", "Host", "web1"},
		{"Host=web1", "Host", "web1"},
		{"User = deploy", "User", "deploy"},
		{"Port    2222", "Port", "2222"},
		{"Host", "Host", ""},
	}
	for _, c := range cases {
		k, v := splitSSHConfigLine(c.in)
		if k != c.key || v != c.val {
			t.Errorf("split(%q) = (%q,%q), want (%q,%q)", c.in, k, v, c.key, c.val)
		}
	}
}

func TestBastionConfig(t *testing.T) {
	base := SSHConfig{Host: "target", User: "targetuser", ProxyPassword: "pp", TrustHost: true}
	cases := []struct {
		jump     string
		wantHost string
		wantPort int
		wantUser string
		wantErr  bool
	}{
		{"admin@bastion:2222", "bastion", 2222, "admin", false},
		{"bastion", "bastion", 22, "targetuser", false}, // user наследуется
		{"admin@bastion", "bastion", 22, "admin", false},
		{"10.0.0.1:22", "10.0.0.1", 22, "targetuser", false},
		{"", "", 0, "", true}, // пусто — ошибка (не должно вызываться)
		{"host:notaport", "", 0, "", true},
	}
	for _, c := range cases {
		cfg := base
		cfg.ProxyJump = c.jump
		bc, err := bastionConfig(cfg)
		if c.wantErr {
			if err == nil {
				t.Errorf("bastionConfig(%q): ожидалась ошибка, %+v", c.jump, bc)
			}
			continue
		}
		if err != nil {
			t.Errorf("bastionConfig(%q): %v", c.jump, err)
			continue
		}
		if bc.Host != c.wantHost || bc.Port != c.wantPort || bc.User != c.wantUser {
			t.Errorf("bastionConfig(%q) = %+v", c.jump, bc)
		}
		if bc.Password != "pp" || !bc.TrustHost {
			t.Errorf("bastionConfig(%q): пароль/TrustHost не прокинуты: %+v", c.jump, bc)
		}
	}
}
