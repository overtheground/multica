import unittest
from reply import split_text

class SplittingTest(unittest.TestCase):
    def test_preserves_entire_long_reply_including_unicode_and_whitespace(self):
        text = ('Привет! Ссылки: https://example.test/a.\n\nКод: `x = 2`. 🎙️ ' * 130) + 'Хвост'
        chunks = split_text(text)
        self.assertGreater(len(chunks), 1)
        self.assertEqual(''.join(chunks), text)
        self.assertTrue(all(0 < len(chunk) <= 1800 for chunk in chunks))

    def test_unbroken_text(self):
        text = 'я' * 5000
        self.assertEqual(''.join(split_text(text)), text)
        self.assertEqual(list(map(len, split_text(text))), [1800, 1800, 1400])

if __name__ == '__main__':
    unittest.main()
