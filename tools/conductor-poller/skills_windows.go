package main

import _ "embed"

//go:embed skills/irisync-design/SKILL.md
var irisyncDesignSkill string

//go:embed skills/irisync-implementation/SKILL.md
var irisyncImplementationSkill string

//go:embed skills/irisync-review/SKILL.md
var irisyncReviewSkill string

func skillInput(skill, task string, payload []byte) []byte {
	return append([]byte(skill+"\n\n今回の作業\n"+task+"\n\n入力データ（JSON）\n"), payload...)
}
