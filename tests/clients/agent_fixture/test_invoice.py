import unittest

from invoice import total_with_tax


class InvoiceTest(unittest.TestCase):
    def test_tax_is_a_fraction_of_subtotal(self):
        self.assertAlmostEqual(total_with_tax(100, 0.19), 119)
        self.assertAlmostEqual(total_with_tax(50, 0.07), 53.5)
        self.assertEqual(total_with_tax(0, 0.19), 0)


if __name__ == "__main__":
    unittest.main()
