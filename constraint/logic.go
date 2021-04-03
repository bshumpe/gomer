package constraint

type logicalOp = string

const (
	andOp logicalOp = "And"
	orOp  logicalOp = "Or"
	notOp logicalOp = "Not"
	noOp  logicalOp = ""

	lcAndOp = "and"
	lcOrOp  = "or"
	lcNotOp = "not"
)

func And(operands ...Constraint) Constraint {
	switch len(operands) {
	case 0:
		return Fail("'And' constraint requires at least one operand")
	case 1:
		return operands[0]
	}

	return &constraint{andOp, operands, func(toTest interface{}) bool {
		for _, operand := range operands {
			if !operand.Test(toTest) {
				return false
			}
		}
		return true
	}}
}

func Or(operands ...Constraint) Constraint {
	switch len(operands) {
	case 0:
		return Fail("'Or' constraint requires at least one operand")
	case 1:
		return operands[0]
	}

	return &constraint{orOp, operands, func(toTest interface{}) bool {
		for _, operand := range operands {
			if operand.Test(toTest) {
				return true
			}
		}
		return false
	}}
}

func Not(operand Constraint) Constraint {
	return &constraint{notOp, operand, func(toTest interface{}) bool {
		return !operand.Test(toTest)
	}}
}
