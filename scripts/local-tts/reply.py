#!/usr/bin/env python3
"""Synthesize a new reply locally; optionally bind it through the Multica CLI."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import wave
from fractions import Fraction

ROOT = Path('/home/desktop/local-tts')
MAX_CHARS = 1800


def split_text(text, limit=MAX_CHARS):
    # Preserve every character; prefer sentence/word boundaries for speech.
    result = []
    while len(text) > limit:
        cut = max(text.rfind('\n', 0, limit), text.rfind('. ', 0, limit), text.rfind(' ', 0, limit))
        cut = cut + 1 if cut > limit // 2 else limit
        result.append(text[:cut])
        text = text[cut:]
    if text:
        result.append(text)
    return result


def synthesize(text, directory):
    import av
    import numpy as np
    import onnxruntime as ort
    from piper import PiperVoice
    from piper.config import PiperConfig

    model = ROOT / 'models/ru_RU-irina-medium.onnx'
    opts = ort.SessionOptions()
    opts.intra_op_num_threads = opts.inter_op_num_threads = 1
    voice = PiperVoice(config=PiperConfig.from_dict(json.loads(Path(str(model) + '.json').read_text())),
                       session=ort.InferenceSession(str(model), sess_options=opts, providers=['CPUExecutionProvider']))
    paths = []
    for index, part in enumerate(split_text(text), 1):
        source = directory / 'source.wav'
        with wave.open(str(source), 'wb') as wav:
            voice.synthesize_wav(part, wav)
        with wave.open(str(source), 'rb') as wav:
            rate = wav.getframerate()
            assert wav.getnchannels() == 1 and wav.getsampwidth() == 2
            samples = np.frombuffer(wav.readframes(wav.getnframes()), np.int16).astype(np.float32) / 32768
        # Same chain and gain as the approved +2 sample. Tempo compensation
        # restores duration; only a short processing tail is padded/trimmed.
        pitched_rate = round(rate * 2 ** (2 / 12))
        graph = av.filter.Graph()
        src = graph.add_abuffer(sample_rate=rate, format='fltp', layout='mono', time_base=Fraction(1, rate))
        previous = src
        for name, value in [('asetrate', str(pitched_rate)), ('aresample', str(rate)), ('atempo', str(rate / pitched_rate))]:
            node = graph.add(name, value)
            previous.link_to(node)
            previous = node
        sink = graph.add('abuffersink')
        previous.link_to(sink)
        graph.configure()
        frame = av.AudioFrame.from_ndarray(samples.reshape(1, -1), format='fltp', layout='mono')
        frame.sample_rate = rate
        frame.pts = 0
        frame.time_base = Fraction(1, rate)
        src.push(frame)
        src.push(None)
        frames = []
        while True:
            try:
                frames.append(sink.pull().to_ndarray().reshape(-1))
            except (av.error.EOFError, av.error.BlockingIOError):
                break
        data = np.concatenate(frames)
        data = np.pad(data, (0, max(0, len(samples) - len(data))))[:len(samples)] * 0.75
        path = directory / f'irina-plus2-{index:03d}.voice.ogg'
        with av.open(str(path), 'w', format='ogg') as output:
            stream = output.add_stream('libopus', rate=48000, layout='mono')
            stream.bit_rate = 32000
            resampler = av.AudioResampler(format='s16', layout='mono', rate=48000)
            frame = av.AudioFrame.from_ndarray(data.reshape(1, -1).astype(np.float32), format='fltp', layout='mono')
            frame.sample_rate = rate
            frame.pts = 0
            frame.time_base = Fraction(1, rate)
            for item in resampler.resample(frame) + resampler.resample(None):
                for packet in stream.encode(item):
                    output.mux(packet)
            for packet in stream.encode(None):
                output.mux(packet)
        assert path.stat().st_size < 49 * 1024 * 1024
        paths.append(path)
    source.unlink(missing_ok=True)
    return paths


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--text-file', type=Path, required=True)
    parser.add_argument('--output-dir', type=Path, required=True)
    parser.add_argument('--upload', action='store_true')
    args = parser.parse_args()
    text = args.text_file.read_text(encoding='utf-8')
    if not text.strip():
        raise SystemExit('Empty reply: no audio generated')
    if args.upload and not os.environ.get('MULTICA_TASK_ID'):
        raise SystemExit('Upload requires the current Multica chat task')
    args.output_dir.mkdir(parents=True, exist_ok=True)
    with (ROOT / 'synthesis.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        # New private directory per invocation; never scan/replay older audio.
        directory = Path(tempfile.mkdtemp(prefix='reply-', dir=args.output_dir))
        paths = synthesize(text, directory)
    uploads = []
    if args.upload:
        for path in paths:
            # CLI success is storage/binding acceptance, not Telegram delivery.
            subprocess.run(['multica', 'attachment', 'upload', str(path)], check=True, timeout=60, stdout=subprocess.DEVNULL)
            uploads.append(str(path))
    print(json.dumps({'voice': 'Piper Irina +2', 'text_sha256': hashlib.sha256(text.encode()).hexdigest(),
                      'files': [str(path) for path in paths], 'uploads_accepted': len(uploads),
                      'telegram_delivery': 'not_verified'}, ensure_ascii=False))


if __name__ == '__main__':
    main()
