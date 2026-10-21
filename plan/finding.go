package plan

// ProblemKind names the kind of a Finding. The values are stable (see Finding): they are written to JSON
// and compared by tools.
type ProblemKind string

// The kinds a Finding can have. Each says, in its comment, which names it carries (in order) and what
// Got and Want hold. Kinds may be added in any 0.x release; a kind is never renamed or removed, and its
// Names order and Got and Want formats never change.
const (
	// ProblemEdgeTypes: an edge joins steps of other types. Names are [from, to]; Got is the type the
	// producer gives and Want the type the consumer takes.
	ProblemEdgeTypes ProblemKind = "edge_types"
	// ProblemArmTypes: a switch arm's predicate tests another type than the switched node gives. Names
	// are [switched node, predicate, target]; Got is the type the predicate tests, Want the type the node gives.
	ProblemArmTypes ProblemKind = "arm_types"
	// ProblemJoinInputTypes: a join input does not fit its merge. Names are [join, input]; Got is the type
	// the input gives, Want the type the merge takes there.
	ProblemJoinInputTypes ProblemKind = "join_input_types"
	// ProblemTerminalTypes: a terminal step reached from the entry gives another type than the flow's
	// output. Names are [step]; Got is its type, Want the flow's output type.
	ProblemTerminalTypes ProblemKind = "terminal_types"
	// ProblemSafetyAboveRegistration: a node states a retry safety above what its block registers in Go,
	// which a config may not claim. Names are [node]; Got is the stated safety and Want the registered one.
	ProblemSafetyAboveRegistration ProblemKind = "safety_above_registration"
	// ProblemLoopNoExitArm: a loop's switch has no arm or else that leaves it. Names are [switch].
	ProblemLoopNoExitArm ProblemKind = "loop_no_exit_arm"
	// ProblemLoopMoreThanOneBackArm: a switch has more than one loop-back arm. Names are [switch].
	ProblemLoopMoreThanOneBackArm ProblemKind = "loop_more_than_one_back_arm"
	// ProblemLoopHeadNotAncestor: a loop-back arm targets a node that does not reach its switch. Names
	// are [switch, head].
	ProblemLoopHeadNotAncestor ProblemKind = "loop_head_not_ancestor"
	// ProblemLoopBodyNotSimple: a node lies between a loop's head and switch but is not on the loop path.
	// Names are [switch, node].
	ProblemLoopBodyNotSimple ProblemKind = "loop_body_not_simple"
	// ProblemLoopInnerSwitch: a step inside a loop has a switch of its own. Names are [node, loop switch].
	ProblemLoopInnerSwitch ProblemKind = "loop_inner_switch"
	// ProblemLoopEdgeLeaves: an edge leaves a loop's body other than through its switch's exit arms.
	// Names are [from, to, loop switch].
	ProblemLoopEdgeLeaves ProblemKind = "loop_edge_leaves"
	// ProblemLoopEdgeEnters: an edge enters a loop's body past its head. Names are [from, to, loop switch,
	// head].
	ProblemLoopEdgeEnters ProblemKind = "loop_edge_enters"
	// ProblemOther: every other problem. Names, Got and Want are empty; only Message says what is wrong.
	// An "other" finding may become a specific kind in any 0.x release, so a tool treats "other" and a
	// kind it does not know alike: as an error it cannot classify.
	ProblemOther ProblemKind = "other"
)

// Finding is the structure behind one error in a ConfigReport: what kind of problem it is and the
// names it is about, so that a tool need not read the Message to tell problems apart. A Finding is
// found from the config and the registered types alone, so it is present beside any other error.
//
// Every error in ConfigReport.Problems has exactly one Finding with the same Message (Findings are in
// the order of the errors). The one exception is the line "and N more type problems" that ends a long
// list of type problems: Problems lists the first 100, and Findings has all of them. Warnings have no
// Finding.
//
// A problem is identified by its Kind, Names, Got and Want, never by its Message, which shortens long
// names and names a wiring element by its position. Names hold the names as the config has them, in
// full; they hold no position. Got and Want are empty for a kind that does not say them.
//
// Stability (0.x): a kind's name, the order of its Names and the format of its Got and Want do not
// change; kinds may be added, so a tool must ignore a kind it does not know; "other" may be split into
// new kinds; Message is for people and may change.
type Finding struct {
	Kind    ProblemKind `json:"kind"`
	Names   []string    `json:"names,omitempty"`
	Got     string      `json:"got,omitempty"`
	Want    string      `json:"want,omitempty"`
	Message string      `json:"message"`
}
