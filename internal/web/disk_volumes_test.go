package web

import (
	"testing"

	"github.com/shirou/gopsutil/v4/disk"
)

// Карточка «Диск» показывала самый заполненный том без разбора: на штатной
// Ubuntu это snap-образ на 100%, на Windows — смонтированный ISO (#119).
func TestIsServiceVolume(t *testing.T) {
	tests := []struct {
		name string
		part disk.PartitionStat
		want bool
	}{
		{
			name: "snap",
			part: disk.PartitionStat{Device: "/dev/loop3", Mountpoint: "/snap/core22/1234", Fstype: "squashfs", Opts: []string{"ro", "nodev"}},
			want: true,
		},
		{
			name: "tmpfs",
			part: disk.PartitionStat{Device: "tmpfs", Mountpoint: "/run/user/1000", Fstype: "tmpfs", Opts: []string{"rw"}},
			want: true,
		},
		{
			name: "сетевая шара",
			part: disk.PartitionStat{Device: "//nas/backup", Mountpoint: "/mnt/nas", Fstype: "cifs", Opts: []string{"rw"}},
			want: true,
		},
		{
			name: "смонтированный образ Windows",
			part: disk.PartitionStat{Device: "E:", Mountpoint: "E:", Fstype: "CDFS", Opts: []string{"ro"}},
			want: true,
		},
		{
			name: "корень",
			part: disk.PartitionStat{Device: "/dev/nvme0n1p2", Mountpoint: "/", Fstype: "ext4", Opts: []string{"rw", "relatime"}},
			want: false,
		},
		{
			name: "диск Windows",
			part: disk.PartitionStat{Device: "C:", Mountpoint: "C:", Fstype: "NTFS", Opts: []string{"rw", "compress"}},
			want: false,
		},
	}
	for _, test := range tests {
		if got := isServiceVolume(test.part); got != test.want {
			t.Errorf("%s: isServiceVolume=%v, ожидали %v", test.name, got, test.want)
		}
	}
}

// «Главный» том — тот, где живут рабочие каталоги: побеждает самый длинный
// подходящий mountpoint, а не первый попавшийся.
func TestVolumeOfPathPicksLongestMount(t *testing.T) {
	partitions := []disk.PartitionStat{
		{Mountpoint: "/", Fstype: "ext4"},
		{Mountpoint: "/var", Fstype: "ext4"},
		{Mountpoint: "/home", Fstype: "ext4"},
		{Mountpoint: "/snap/core22/1234", Fstype: "squashfs"},
	}
	if got := volumeOfPath(partitions, "/home/deploy"); got != "/home" {
		t.Fatalf("mount=%q, ожидали /home", got)
	}
	if got := volumeOfPath(partitions, "/opt/app"); got != "/" {
		t.Fatalf("mount=%q, ожидали /", got)
	}
	if got := volumeOfPath(nil, "/home/deploy"); got != "" {
		t.Fatalf("пустой список обязан дать пустой том, получили %q", got)
	}
}

// Кэш дорогой части сводки: второй запрос подряд обязан обслуживаться без
// нового замера, иначе каждый экран и каждое устройство будят ПК своим
// блокирующим cpu.Percent (#122).
func TestHostStatsUsesCache(t *testing.T) {
	hostStatsMu.Lock()
	hostStatsLast = hostStatsSnapshot{}
	hostStatsMu.Unlock()

	first := hostStats()
	second := hostStats()
	if first.at.IsZero() {
		t.Fatal("первый снимок обязан иметь метку времени")
	}
	if !second.at.Equal(first.at) {
		t.Fatalf("повторный запрос пересчитал снимок: %v → %v", first.at, second.at)
	}
}
