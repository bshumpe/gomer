package dynamodb

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbiface"

	"github.com/jt0/gomer/constraint"
	"github.com/jt0/gomer/crypto"
	"github.com/jt0/gomer/data"
	"github.com/jt0/gomer/data/dataerr"
	"github.com/jt0/gomer/flect"
	"github.com/jt0/gomer/gomerr"
	"github.com/jt0/gomer/limit"
)

type table struct {
	index
	tableName              *string
	ddb                    dynamodbiface.DynamoDBAPI
	defaultLimit           *int64
	maxLimit               *int64
	defaultConsistencyType ConsistencyType
	indexes                map[string]*index
	persistableTypes       map[string]*persistableType
	valueSeparatorChar     byte
	nextTokenizer          nextTokenizer
	failDeleteIfNotPresent bool
}

type Configuration struct {
	DynamoDb               dynamodbiface.DynamoDBAPI
	MaxResultsDefault      int64
	MaxResultsMax          int64
	ConsistencyDefault     ConsistencyType
	ValueSeparatorChar     byte
	QueryWildcardChar      byte
	NextTokenCipher        crypto.Cipher
	FailDeleteIfNotPresent bool
}

var tables = make(map[string]data.Store)

type ConsistencyType int

const (
	Indifferent ConsistencyType = iota
	Required
	Preferred

	SymbolChars                    = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`"
	ValueSeparatorCharDefault      = ':'
	QueryWildcardCharDefault  byte = 0
)

const maxItemSize = limit.DataSize(400 * 1024)

type ConsistencyTyper interface {
	ConsistencyType() ConsistencyType
	SetConsistencyType(consistencyType ConsistencyType)
}

type ItemResolver func(*index, any) (any, gomerr.Gomerr)

func Store(tableName string, config *Configuration /* resolver data.ItemResolver,*/, persistables ...data.Persistable) (store data.Store, ge gomerr.Gomerr) {
	t := &table{
		tableName:              &tableName,
		index:                  index{canReadConsistently: true},
		ddb:                    config.DynamoDb,
		defaultLimit:           &config.MaxResultsDefault,
		maxLimit:               &config.MaxResultsMax,
		defaultConsistencyType: config.ConsistencyDefault,
		indexes:                make(map[string]*index),
		persistableTypes:       make(map[string]*persistableType),
		nextTokenizer:          nextTokenizer{cipher: config.NextTokenCipher},
		failDeleteIfNotPresent: config.FailDeleteIfNotPresent,
	}

	if t.valueSeparatorChar, ge = validOrDefaultChar(config.ValueSeparatorChar, ValueSeparatorCharDefault); ge != nil {
		return nil, ge
	}

	if t.queryWildcardChar, ge = validOrDefaultChar(config.QueryWildcardChar, QueryWildcardCharDefault); ge != nil {
		return nil, ge
	}

	if ge = t.prepare(persistables); ge != nil {
		return nil, ge
	}

	tables[tableName] = t

	return t, nil
}

func validOrDefaultChar(ch byte, _default byte) (byte, gomerr.Gomerr) {
	if ch != 0 {
		s := string(ch)
		if strings.Contains(SymbolChars, s) {
			return ch, nil
		} else {
			return 0, gomerr.Configuration("QueryWildcardChar " + s + " not in the valid set: " + SymbolChars)
		}
	} else {
		return _default, nil
	}
}

func Stores() map[string]data.Store {
	return tables
}

func (t *table) prepare(persistables []data.Persistable) gomerr.Gomerr {
	input := &dynamodb.DescribeTableInput{TableName: t.tableName}
	output, err := t.ddb.DescribeTable(input)
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			switch awsErr.Code() {
			case dynamodb.ErrCodeResourceNotFoundException:
				return gomerr.Unprocessable("Table", *t.tableName).Wrap(awsErr)
			}
		}

		return gomerr.Dependency("DynamoDB", input).Wrap(err)
	}

	attributeTypes := make(map[string]string)
	for _, at := range output.Table.AttributeDefinitions {
		attributeTypes[*at.AttributeName] = *at.AttributeType
	}

	if ge := t.index.processKeySchema(output.Table.KeySchema, attributeTypes); ge != nil {
		return ge
	}

	t.indexes[""] = &t.index

	for _, lsid := range output.Table.LocalSecondaryIndexes {
		lsi := &index{
			name:                lsid.IndexName,
			canReadConsistently: true,
			valueSeparatorChar:  t.valueSeparatorChar,
			queryWildcardChar:   t.queryWildcardChar,
		}

		if ge := lsi.processKeySchema(lsid.KeySchema, attributeTypes); ge != nil {
			return ge
		}

		lsi.pk = t.pk // Overwrite w/ t.pk

		t.indexes[*lsid.IndexName] = lsi
	}

	for _, gsid := range output.Table.GlobalSecondaryIndexes {
		gsi := &index{
			name:                gsid.IndexName,
			canReadConsistently: false,
			valueSeparatorChar:  t.valueSeparatorChar,
			queryWildcardChar:   t.queryWildcardChar,
		}

		if ge := gsi.processKeySchema(gsid.KeySchema, attributeTypes); ge != nil {
			return ge
		}

		t.indexes[*gsid.IndexName] = gsi
	}

	for _, persistable := range persistables {
		pType := reflect.TypeOf(persistable)
		pElem := pType.Elem()

		unqualifiedPersistableName := pElem.String()
		unqualifiedPersistableName = unqualifiedPersistableName[strings.Index(unqualifiedPersistableName, ".")+1:]

		pt, ge := newPersistableType(t, unqualifiedPersistableName, pElem)
		if ge != nil {
			return ge
		}

		// Validate that each key in each index has fully defined key fields for this persistable
		for _, idx := range t.indexes {
			for _, attribute := range idx.keyAttributes() {
				if keyFields := attribute.keyFieldsByPersistable[unqualifiedPersistableName]; keyFields != nil {
					for i, kf := range keyFields {
						if kf == nil {
							return gomerr.Configuration(
								fmt.Sprintf("Index %s is missing a key field: %s[%s][%d]", idx.friendlyName(), attribute.name, unqualifiedPersistableName, i),
							).AddAttribute("keyFields", keyFields)
						}
					}
				} else {
					attribute.keyFieldsByPersistable[unqualifiedPersistableName] = []*keyField{{name: pt.dbNameToFieldName(attribute.name), ascending: &trueVal}}
				}
			}
		}

		t.persistableTypes[unqualifiedPersistableName] = pt
	}

	return nil
}

func (t *table) Name() string {
	return *t.tableName
}

func (t *table) Create(p data.Persistable) (ge gomerr.Gomerr) {
	defer func() {
		if ge != nil {
			// Todo: is this needed or should this just be added to the attributes?
			ge = dataerr.Store("Create", p).Wrap(ge)
		}
	}()

	ge = t.put(p, t.persistableTypes[p.TypeName()].fieldConstraints, true)

	return
}

func (t *table) Update(p data.Persistable, update data.Persistable) (ge gomerr.Gomerr) {
	defer func() {
		if ge != nil {
			ge = dataerr.Store("Update", p).Wrap(ge)
		}
	}()

	// TODO:p1 support partial update vs put()

	fieldConstraintsToCheck := make(map[string]constraint.Constraint)
	if update != nil {
		uv := reflect.ValueOf(update).Elem()
		pv := reflect.ValueOf(p).Elem()

		for i := 0; i < uv.NumField(); i++ {
			uField := uv.Field(i)
			// TODO:p0 Support structs. Will want to recurse through and not bother w/ CanSet() checks until we know
			//         we're dealing w/ a scalar.
			if !uField.CanSet() || uField.Kind() == reflect.Struct || (uField.Kind() == reflect.Ptr && uField.Elem().Kind() == reflect.Struct) {
				continue
			}

			pField := pv.Field(i)
			if reflect.DeepEqual(uField.Interface(), pField.Interface()) {
				uField.Set(reflect.Zero(uField.Type()))
			} else if uField.Kind() == reflect.Ptr {
				if uField.IsNil() {
					continue
				}
				if !pField.IsNil() && reflect.DeepEqual(uField.Elem().Interface(), pField.Elem().Interface()) {
					uField.Set(reflect.Zero(uField.Type()))
				} else {
					pField.Set(uField)
				}
			} else {
				if uField.IsZero() {
					continue
				}
				pField.Set(uField)
			}
		}

	nextCondition:
		for fieldName, fieldConstraint := range t.persistableTypes[p.TypeName()].fieldConstraints {
			// Test if the field with the constraint has been updated. If so, add the constraint and continue.
			if !uv.FieldByName(fieldName).IsZero() {
				fieldConstraintsToCheck[fieldName] = fieldConstraint
				continue nextCondition
			}

			// See if any of the other fields that are used to determine uniqueness have been updated. If yes, add the
			// condition to the list and continue to the next condition.
			for _, otherField := range fieldConstraint.Parameters().([]string) {
				uField := uv.FieldByName(otherField)
				if !uField.IsZero() /* TODO: remove rest once structs supported above */ && uField.Interface() != pv.FieldByName(otherField).Interface() {
					fieldConstraintsToCheck[fieldName] = fieldConstraint
					continue nextCondition
				}
			}

		}
	}

	ge = t.put(p, fieldConstraintsToCheck, false)

	return
}

func (t *table) put(p data.Persistable, fieldConstraints map[string]constraint.Constraint, ensureUniqueId bool) gomerr.Gomerr {
	for fieldName, fieldConstraint := range fieldConstraints {
		if ge := fieldConstraint.Validate(fieldName, p); ge != nil {
			return ge
		}
	}

	av, err := dynamodbattribute.MarshalMap(p)
	if err != nil {
		return gomerr.Marshal(p.TypeName(), p).Wrap(err)
	}

	t.persistableTypes[p.TypeName()].convertFieldNamesToDbNames(&av)

	for _, idx := range t.indexes {
		_ = idx.populateKeyValues(av, p, false)
	}

	// TODO: here we could compare the current av map w/ one we stashed into the object somewhere

	var uniqueIdConditionExpression *string
	if ensureUniqueId {
		expression := fmt.Sprintf("attribute_not_exists(%s)", t.pk.name)
		if t.sk != nil {
			expression += fmt.Sprintf(" AND attribute_not_exists(%s)", t.sk.name)
		}
		uniqueIdConditionExpression = &expression
	}

	// TODO:p1 optimistic locking

	input := &dynamodb.PutItemInput{
		Item:                av,
		TableName:           t.tableName,
		ConditionExpression: uniqueIdConditionExpression,
	}

	_, err = t.ddb.PutItem(input) // TODO:p3 look at result data to track capacity or other info?
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			switch awsErr.Code() {
			case dynamodb.ErrCodeConditionalCheckFailedException:
				if ensureUniqueId {
					return gomerr.Internal("Unique id check failed, retry with a new id value").Wrap(err)
				} else {
					return gomerr.Dependency("DynamoDB", input).Wrap(err)
				}
			case dynamodb.ErrCodeRequestLimitExceeded, dynamodb.ErrCodeProvisionedThroughputExceededException:
				return limit.UnquantifiedExcess("DynamoDB", "throughput").Wrap(awsErr)
			case dynamodb.ErrCodeItemCollectionSizeLimitExceededException:
				return limit.Exceeded("DynamoDB", "item.size()", maxItemSize, limit.NotApplicable, limit.Unknown)
			}
		}

		return gomerr.Dependency("DynamoDB", input).Wrap(err)
	}

	return nil
}

func (t *table) Read(p data.Persistable) (ge gomerr.Gomerr) {
	defer func() {
		if ge != nil {
			ge = dataerr.Store("Read", p).Wrap(ge)
		}
	}()

	key := make(map[string]*dynamodb.AttributeValue, 2)
	ge = t.populateKeyValues(key, p, true)
	if ge != nil {
		return ge
	}

	input := &dynamodb.GetItemInput{
		Key:            key,
		ConsistentRead: consistentRead(t.consistencyType(p), true),
		TableName:      t.tableName,
	}
	output, err := t.ddb.GetItem(input)
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			switch awsErr.Code() {
			case dynamodb.ErrCodeResourceNotFoundException:
				return dataerr.PersistableNotFound(p.TypeName(), key).Wrap(err)
			case dynamodb.ErrCodeRequestLimitExceeded, dynamodb.ErrCodeProvisionedThroughputExceededException:
				return limit.UnquantifiedExcess("DynamoDB", "throughput").Wrap(awsErr)
			}
		}

		return gomerr.Dependency("DynamoDB", input).Wrap(err)
	}

	if output.Item == nil {
		return dataerr.PersistableNotFound(p.TypeName(), key)
	}

	err = dynamodbattribute.UnmarshalMap(output.Item, p)
	if err != nil {
		return gomerr.Unmarshal(p.TypeName(), output.Item, p).Wrap(err)
	}

	ge = t.populateKeyFields(p, output.Item)
	if ge != nil {
		return ge
	}

	return nil
}

func (t *table) Delete(p data.Persistable) (ge gomerr.Gomerr) {
	defer func() {
		if ge != nil {
			ge = dataerr.Store("Delete", p).Wrap(ge)
		}
	}()

	// TODO:p2 support a soft-delete option

	key := make(map[string]*dynamodb.AttributeValue, 2)
	ge = t.populateKeyValues(key, p, true)
	if ge != nil {
		return ge
	}

	var existenceCheckExpression *string
	if t.failDeleteIfNotPresent {
		expression := fmt.Sprintf("attribute_exists(%s)", t.pk.name)
		if t.sk != nil {
			expression += fmt.Sprintf(" AND attribute_exists(%s)", t.sk.name)
		}
		existenceCheckExpression = &expression
	}

	input := &dynamodb.DeleteItemInput{
		Key:                 key,
		TableName:           t.tableName,
		ConditionExpression: existenceCheckExpression,
	}
	_, err := t.ddb.DeleteItem(input)
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			switch awsErr.Code() {
			case dynamodb.ErrCodeResourceNotFoundException, dynamodb.ErrCodeConditionalCheckFailedException:
				return dataerr.PersistableNotFound(p.TypeName(), key).Wrap(err)
			case dynamodb.ErrCodeRequestLimitExceeded, dynamodb.ErrCodeProvisionedThroughputExceededException:
				return limit.UnquantifiedExcess("DynamoDB", "throughput").Wrap(awsErr)
			}
		}

		return gomerr.Dependency("DynamoDB", input).Wrap(err)
	}

	return nil
}

func (t *table) List(q data.Listable) (ge gomerr.Gomerr) {
	defer func() {
		if ge != nil {
			ge = dataerr.Store("List", q).Wrap(ge)
		}
	}()

	var idx *index
	var input *dynamodb.QueryInput
	idx, input, ge = t.buildQueryInput(q, q.TypeNames()[0]) // TODO:p2 Fix when query supports multiple types
	if ge != nil {
		return ge
	}

	var output *dynamodb.QueryOutput
	output, ge = t.runQuery(input)
	if ge != nil {
		return ge
	}

	nt, ge := t.nextTokenizer.tokenize(q, output.LastEvaluatedKey)
	if ge != nil {
		return gomerr.Internal("Unable to generate nextToken").Wrap(ge)
	}

	items := make([]any, len(output.Items))
	for i, item := range output.Items {
		if items[i], ge = t.persistableTypes[q.TypeOf(item)].resolver(idx, item); ge != nil {
			return ge
		}
	}

	q.SetItems(items)
	q.SetNextPageToken(nt)

	return nil
}

func (t *table) isFieldTupleUnique(fields []string) func(pi interface{}) gomerr.Gomerr {
	return func(pi interface{}) gomerr.Gomerr {
		p, ok := pi.(data.Persistable)
		if !ok {
			return gomerr.Unprocessable("Test value is not a data.Persistable", pi)
		}

		q := p.NewListable()
		if ct, ok := q.(ConsistencyTyper); ok {
			ct.SetConsistencyType(Preferred)
		}

		qv := reflect.ValueOf(q).Elem()
		pv := reflect.ValueOf(p).Elem()
		for _, field := range fields {
			qv.FieldByName(field).Set(pv.FieldByName(field))
		}

		_, input, ge := t.buildQueryInput(q, p.TypeName())
		if ge != nil {
			return ge
		}

		for queryLimit := int64(1); queryLimit <= 300; queryLimit += 100 { // Bump limit up each time
			input.Limit = &queryLimit

			output, queryErr := t.runQuery(input)
			if queryErr != nil {
				return queryErr
			}

			if len(output.Items) > 0 {
				newP := reflect.New(pv.Type()).Interface()
				err := dynamodbattribute.UnmarshalMap(output.Items[0], newP)
				return constraint.NotSatisfied(pi).AddAttribute("Existing", newP).Wrap(err)
			}

			if output.LastEvaluatedKey == nil {
				return nil
			}

			input.ExclusiveStartKey = output.LastEvaluatedKey
		}

		return gomerr.Unprocessable("Too many db checks to verify uniqueness constraint", pi)
	}
}

type UniqueConstraint struct {
	constraint.Constraint
}

// buildQueryInput Builds the DynamoDB QueryInput types based on the provided queryable. See indexFor and
// nextTokenizer.untokenize for possible error types.
func (t *table) buildQueryInput(q data.Listable, persistableTypeName string) (*index, *dynamodb.QueryInput, gomerr.Gomerr) {
	idx, ascending, consistent, ge := indexFor(t, q)
	if ge != nil {
		return nil, nil, ge
	}

	expressionAttributeNames := make(map[string]*string, 2)
	expressionAttributeValues := make(map[string]*dynamodb.AttributeValue, 2)

	// TODO: any reason Elem() would be incorrect?
	qElem := reflect.ValueOf(q).Elem()

	keyConditionExpression := safeName(idx.pk.name, expressionAttributeNames) + "=:pk"
	expressionAttributeValues[":pk"] = idx.pk.attributeValue(qElem, persistableTypeName, t.valueSeparatorChar, 0) // Non-null because indexFor succeeded

	// TODO: customers should opt-in to wildcard matches on a field-by-field basis
	// TODO: need to provide a way to sanitize, both when saving and querying data, the delimiter char
	if idx.sk != nil {
		if eav := idx.sk.attributeValue(qElem, persistableTypeName, t.valueSeparatorChar, t.queryWildcardChar); eav != nil {
			if eav.S != nil && len(*eav.S) > 0 && ((*eav.S)[len(*eav.S)-1] == t.queryWildcardChar || (*eav.S)[len(*eav.S)-1] == t.valueSeparatorChar) {
				*eav.S = (*eav.S)[:len(*eav.S)-1] // remove the last char
				keyConditionExpression += " AND begins_with(" + safeName(idx.sk.name, expressionAttributeNames) + ",:sk)"
			} else {
				if qt, ok := q.(data.QueryTyper); ok {
					switch qt.QueryType() {
					case data.GT:
						keyConditionExpression += " AND " + safeName(idx.sk.name, expressionAttributeNames) + ">:sk"
					case data.GTE:
						keyConditionExpression += " AND " + safeName(idx.sk.name, expressionAttributeNames) + ">=:sk"
					case data.LT:
						keyConditionExpression += " AND " + safeName(idx.sk.name, expressionAttributeNames) + "<:sk"
					case data.LTE:
						keyConditionExpression += " AND " + safeName(idx.sk.name, expressionAttributeNames) + "<=:sk"
					case data.EQ:
						fallthrough
					default:
						keyConditionExpression += " AND " + safeName(idx.sk.name, expressionAttributeNames) + "=:sk"
					}
				} else {
					keyConditionExpression += " AND " + safeName(idx.sk.name, expressionAttributeNames) + "=:sk"
				}
			}
			expressionAttributeValues[":sk"] = eav
		}
	}

	var fe string
	if fe, ge = t.filterExpression(q, idx, persistableTypeName, expressionAttributeNames, expressionAttributeValues); ge != nil {
		return nil, nil, ge
	}

	var filterExpression *string
	if fe != "" {
		filterExpression = &fe
	}

	// for _, attribute := range q.ResponseFields() {
	// 	safeName(attribute, expressionAttributeNames)
	// }

	if len(expressionAttributeNames) == 0 {
		expressionAttributeNames = nil
	}

	// TODO:p2 projectionExpression
	// var projectionExpressionPtr *string
	// projectionExpression := strings.Join(attributes, ",") // Join() returns "" if len(attributes) == 0
	// if projectionExpression != "" {
	// 	projectionExpressionPtr = &projectionExpression
	// }

	exclusiveStartKey, ge := t.nextTokenizer.untokenize(q)
	if ge != nil {
		return nil, nil, ge
	}

	if q.Ascending() != nil {
		ascending = q.Ascending()
	}

	input := &dynamodb.QueryInput{
		TableName:                 t.tableName,
		IndexName:                 idx.name,
		ConsistentRead:            consistent,
		ExpressionAttributeNames:  expressionAttributeNames,
		ExpressionAttributeValues: expressionAttributeValues,
		KeyConditionExpression:    &keyConditionExpression,
		FilterExpression:          filterExpression,
		ExclusiveStartKey:         exclusiveStartKey,
		Limit:                     t.limit(q.MaximumPageSize()),
		ScanIndexForward:          ascending,
		// ProjectionExpression:      projectionExpressionPtr,
	}

	return idx, input, nil
}

func (t *table) filterExpression(q data.Listable, idx *index, persistableTypeName string, expressionAttributeNames map[string]*string, expressionAttributeValues map[string]*dynamodb.AttributeValue) (string, gomerr.Gomerr) {
	qv, ge := flect.IndirectValue(q, false)
	if ge != nil {
		return "", ge
	}

	keyFields := map[string]bool{}
	for _, ka := range idx.keyAttributes() {
		for _, kf := range ka.keyFieldsByPersistable[persistableTypeName] {
			keyFields[kf.name] = true
		}
	}

	var exp string
	qt := qv.Type()
	for i := 0; i < qt.NumField(); i++ {
		var qfv reflect.Value
		var sf reflect.StructField
		if sf = qt.Field(i); keyFields[sf.Name] {
			continue
		} else if qfv = qv.Field(i); qfv.IsZero() {
			continue
		}
		if qfv.Kind() == reflect.Ptr {
			qfv = qfv.Elem()
		}
		if qfv.Kind() == reflect.Struct {
			continue
		}
		s := fmt.Sprint(qfv.Interface())
		if len(s) == 0 {
			continue
		}
		if len(exp) > 0 {
			exp += " AND "
		}
		filterAlias := ":f" + strconv.Itoa(i)
		if s[len(s)-1] == t.queryWildcardChar {
			s = s[:len(s)-1]
			exp += "begins_with(" + safeName(sf.Name, expressionAttributeNames) + "," + filterAlias + ")"
		} else {
			exp += safeName(sf.Name, expressionAttributeNames) + "=" + filterAlias
		}
		expressionAttributeValues[filterAlias] = &dynamodb.AttributeValue{S: &s}
	}

	return exp, nil
}

func (t *table) runQuery(input *dynamodb.QueryInput) (*dynamodb.QueryOutput, gomerr.Gomerr) {
	output, err := t.ddb.Query(input)
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			// TODO: improve exceptions
			switch awsErr.Code() {
			case dynamodb.ErrCodeRequestLimitExceeded, dynamodb.ErrCodeProvisionedThroughputExceededException:
				return nil, limit.UnquantifiedExcess("DynamoDB", "throughput").Wrap(awsErr)
			case dynamodb.ErrCodeResourceNotFoundException:
				if input.IndexName != nil {
					return nil, gomerr.Unprocessable("Table Index", *input.IndexName).Wrap(awsErr)
				} else {
					return nil, gomerr.Unprocessable("Table", *t.tableName).Wrap(awsErr)
				}
			}
		}

		return nil, gomerr.Dependency("DynamoDB", input).Wrap(err)
	}

	return output, nil
}

func (t *table) consistencyType(p data.Persistable) ConsistencyType {
	if ct, ok := p.(ConsistencyTyper); ok {
		return ct.ConsistencyType()
	} else {
		return t.defaultConsistencyType
	}
}

func (t *table) limit(maximumPageSize int) *int64 {
	if maximumPageSize > 0 && t.maxLimit != nil {
		mps64 := int64(maximumPageSize)
		if mps64 <= *t.maxLimit {
			return &mps64
		} else {
			return t.maxLimit
		}
	} else {
		return t.defaultLimit
	}
}

func safeName(attributeName string, expressionAttributeNames map[string]*string) string {
	// TODO: calculate once and store in persistableType
	if reservedWords[strings.ToUpper(attributeName)] || strings.ContainsAny(attributeName, ". ") || attributeName[0] >= '0' || attributeName[0] <= '9' {
		replacement := "#a" + strconv.Itoa(len(expressionAttributeNames))
		expressionAttributeNames[replacement] = &attributeName
		return replacement
	}
	return attributeName
}

var (
	trueVal  = true
	falseVal = false
)

func consistentRead(consistencyType ConsistencyType, canReadConsistently bool) *bool {
	switch consistencyType {
	case Indifferent:
		return &falseVal
	case Required:
		return &trueVal
	case Preferred:
		return &canReadConsistently
	default:
		return nil
	}
}
