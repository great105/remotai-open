// Package audio — системный звук компьютера для удалённого экрана.
//
// Зачем вообще: у удалённого рабочего стола не было звука вовсе (Opus в
// проекте есть, но только у виртуального браузера на Linux). Смотреть видео,
// слышать уведомление или проверить, что запись пошла, было нельзя — а без
// этого «я как будто за тем компьютером» не работает.
//
// Что берём и что отдаём. Windows отдаёт микшер как float32 48 кГц стерео;
// зрителю столько не нужно и не по карману: сырой PCM в этом формате — это
// 1,5 Мбит/с рядом с 2,6 Мбит/с видео. Поэтому здесь же, до отправки, поток
// сводится в моно 24 кГц s16 (384 кбит/с) и режется на куски по 20 мс.
//
// Opus дал бы ещё в десять раз меньше, но требует библиотеки, которой на
// Windows у нас пока нет (`FindOpusLib` ищет только libopus.so). Когда она
// появится, кодек встанет за этим же преобразованием — формат кусков от него
// не зависит.
package audio

import "encoding/binary"

const (
	// SampleRate — частота, в которой звук уходит зрителю.
	SampleRate = 24000
	// Channels — моно: системный звук слушают «что там происходит», а не
	// ради стереопанорамы, зато трафик вдвое меньше.
	Channels = 1
	// FrameMillis — длина куска. 20 мс — компромисс между накладными
	// расходами на пакет и задержкой.
	FrameMillis = 20
	// FrameSamples — сэмплов в куске (24 000 × 20 мс).
	FrameSamples = SampleRate * FrameMillis / 1000
	// silenceFloor — ниже этого уровня кусок считается тишиной и не
	// отправляется вовсе: молчащий компьютер не должен занимать канал.
	// 40 из 32767 — это примерно −58 дБFS, ниже порога слышимости в шуме.
	silenceFloor = 40
)

// DownmixResample сводит float32-кадры произвольной раскладки в моно s16
// нужной частоты. Возвращает добавленные сэмплы.
//
// Ресемплинг — усреднение соседних сэмплов (48 → 24 кГц это ровно пара).
// Для системного звука этого достаточно; честный полифазный фильтр здесь был
// бы дороже и по коду, и по процессору, а слышимой разницы не даёт.
func DownmixResample(src []float32, channels, srcRate int, dst []int16) []int16 {
	if channels <= 0 || srcRate <= 0 || len(src) == 0 {
		return dst
	}
	frames := len(src) / channels
	if frames == 0 {
		return dst
	}
	step := srcRate / SampleRate
	if step < 1 {
		step = 1
	}
	for i := 0; i+step <= frames; i += step {
		var acc float32
		for s := 0; s < step; s++ {
			for c := 0; c < channels; c++ {
				acc += src[(i+s)*channels+c]
			}
		}
		acc /= float32(step * channels)
		dst = append(dst, clampToInt16(acc))
	}
	return dst
}

// clampToInt16 переводит float32 (−1…1) в s16 с ограничением: микшер Windows
// умеет выдавать значения за пределами диапазона, и без ограничения они
// заворачиваются в противоположный знак — это громкий щелчок в ухо.
func clampToInt16(v float32) int16 {
	s := v * 32767
	if s > 32767 {
		return 32767
	}
	if s < -32768 {
		return -32768
	}
	return int16(s)
}

// IsSilent — можно ли не отправлять этот кусок.
func IsSilent(pcm []int16) bool {
	for _, s := range pcm {
		if s > silenceFloor || s < -silenceFloor {
			return false
		}
	}
	return true
}

// PackPCM упаковывает кусок в little-endian байты для отправки.
func PackPCM(pcm []int16) []byte {
	out := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}
