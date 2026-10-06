import os
import unittest

import stage_task_runner as runner


class StageTaskRunnerTests(unittest.TestCase):
    def test_child_environment_keeps_only_minimal_runtime_paths_and_nvidia_key(self):
        source = {
            "PATH": "/usr/bin",
            "HOME": "/tmp/home",
            "TMPDIR": "/tmp",
            "GOCACHE": "/tmp/go-cache",
            "OPENAI_API_KEY": "DO_NOT_FORWARD",
            "GROQ_API_KEY": "DO_NOT_FORWARD",
            "NVIDIA_API_KEY": "AMBIENT_DO_NOT_FORWARD",
            "RUNSTEAD_SCRIPTED_RESPONSES": "/tmp/DO_NOT_FORWARD",
        }
        child = runner.child_environment("EXPLICIT_KEY_SENTINEL", source)
        self.assertEqual(child, {
            "PATH": "/usr/bin",
            "HOME": "/tmp/home",
            "TMPDIR": "/tmp",
            "GOCACHE": "/tmp/go-cache",
            "NVIDIA_API_KEY": "EXPLICIT_KEY_SENTINEL",
        })

    def test_stage_commands_pin_provider_acceptance_and_disable_retry(self):
        args = runner.RunArgs(
            binary="/tmp/runstead", workspace="/tmp/workspace",
            providers="providers.json", stage2_profile="stage2-profile.json",
            stage3_profile="stage3-profile.json", stage2_acceptance="stage2-acceptance.json",
            stage3_acceptance="stage3-acceptance.json", recipes="recipes.json",
            state_dir="/tmp/state", env_file=runner.EXPECTED_ENV_FILE,
        )
        for stage in ("stage2", "stage3"):
            with self.subTest(stage=stage):
                command = runner.build_command(stage, args)
                self.assertEqual(command[1], "run")
                self.assertIn("nvidia-nim-nemotron-3-super-120b-a12b-canary-v1", command)
                self.assertIn("--retry-policy", command)
                self.assertEqual(command[command.index("--retry-policy") + 1], "off")
                self.assertNotIn("--scripted", command)
                self.assertNotIn("--omniroute", command)
                self.assertIn("--acceptance", command)
        self.assertEqual(runner._task_id(b"task: cli-123456789\nother output"), "cli-123456789")
        self.assertEqual(runner._task_id(b"raw provider text without a task line"), "unavailable")


if __name__ == "__main__":
    unittest.main()
