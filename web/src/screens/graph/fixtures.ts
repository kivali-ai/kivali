// Graph fixtures: the Go golden files, checked against the generated types with `satisfies`.
import goGraphJson from '../../../../internal/web/apitypes/testdata/graph.json';
import goGraphNodeJson from '../../../../internal/web/apitypes/testdata/graph_node.json';
import type { Graph, GraphNode } from '../../api/types.gen';
import type { Wire } from '../work/fixtures';

export const goGraph = (goGraphJson satisfies Wire<Graph>) as Graph;
export const goGraphNode = (goGraphNodeJson satisfies Wire<GraphNode>) as GraphNode;
