import json
import unittest

import stage3_audit


class Stage3AuditTests(unittest.TestCase):
    def test_write_scope_is_exact_app_calc(self):
        self.assertTrue(stage3_audit.scoped("app/calc.go"))
        self.assertFalse(stage3_audit.scoped("app/calc_test.go"))
        self.assertFalse(stage3_audit.scoped("README.md"))

    def test_recipe_pass_requires_declared_started_untruncated_zero_exit(self):
        good = {"recipe_id": "test", "started": True, "exit_code": 0,
                "timed_out": False, "canceled": False, "stdout_truncated": False,
                "stderr_truncated": False}
        self.assertTrue(stage3_audit.recipe_passed(json.dumps(good)))
        self.assertFalse(stage3_audit.recipe_passed(json.dumps({**good, "recipe_id": "other"})))
        self.assertFalse(stage3_audit.recipe_passed(json.dumps({**good, "exit_code": 1})))


if __name__ == "__main__":
    unittest.main()
